// SPDX-License-Identifier: Apache-2.0

package kafka

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	synclib "github.com/xataio/pgstream/internal/sync"
	syncmocks "github.com/xataio/pgstream/internal/sync/mocks"
	"github.com/xataio/pgstream/pkg/kafka"
	kafkamocks "github.com/xataio/pgstream/pkg/kafka/mocks"
	loglib "github.com/xataio/pgstream/pkg/log"
	"github.com/xataio/pgstream/pkg/schemalog"
	"github.com/xataio/pgstream/pkg/wal"
	"github.com/xataio/pgstream/pkg/wal/checkpointer"

	"golang.org/x/sync/semaphore"
)

var (
	testSchema = "test_schema"
	testTable  = "test_table"

	testLSNStr = "1/CF54A048"

	errTest = errors.New("oh noes")
)

func TestBatchKafkaWriter_ProcessWALEvent(t *testing.T) {
	t.Parallel()

	testWalEvent := &wal.Event{
		Data: &wal.Data{
			Action: "I",
			LSN:    testLSNStr,
			Schema: testSchema,
			Table:  testTable,
		},
		CommitPosition: wal.CommitPosition(testLSNStr),
	}

	testCommitPosition := wal.CommitPosition(testLSNStr)

	testBytes := []byte("test")
	mockMarshaler := func(any) ([]byte, error) { return testBytes, nil }

	tests := []struct {
		name            string
		walEvent        *wal.Event
		eventSerialiser func(any) ([]byte, error)
		semaphore       synclib.WeightedSemaphore

		wantMsgs []*msg
		wantErr  error
	}{
		{
			name:     "ok",
			walEvent: testWalEvent,

			wantMsgs: []*msg{
				{
					msg: kafka.Message{
						Key:   []byte(testSchema),
						Value: testBytes,
					},
					pos: testCommitPosition,
				},
			},
			wantErr: nil,
		},
		{
			name: "ok - keep alive",
			walEvent: &wal.Event{
				CommitPosition: testCommitPosition,
			},

			wantMsgs: []*msg{
				{
					pos: testCommitPosition,
				},
			},
			wantErr: nil,
		},
		{
			name: "ok - pgstream schema event",
			walEvent: &wal.Event{
				Data: &wal.Data{
					Action: "I",
					LSN:    testLSNStr,
					Schema: schemalog.SchemaName,
					Table:  schemalog.TableName,
					Columns: []wal.Column{
						{Name: "schema_name", Value: testSchema},
					},
				},
				CommitPosition: testCommitPosition,
			},

			wantMsgs: []*msg{
				{
					msg: kafka.Message{
						Key:   []byte(testSchema),
						Value: testBytes,
					},
					pos: testCommitPosition,
				},
			},
			wantErr: nil,
		},
		{
			name:            "ok - wal event too large, message dropped",
			walEvent:        testWalEvent,
			eventSerialiser: func(any) ([]byte, error) { return []byte(strings.Repeat("a", 101)), nil },

			wantMsgs: []*msg{},
			wantErr:  nil,
		},
		{
			name:            "error - marshaling event",
			walEvent:        testWalEvent,
			eventSerialiser: func(any) ([]byte, error) { return nil, errTest },

			wantMsgs: []*msg{},
			wantErr:  errTest,
		},
		{
			name: "panic recovery - invalid schema value type",
			walEvent: &wal.Event{
				Data: &wal.Data{
					Action: "I",
					LSN:    testLSNStr,
					Schema: schemalog.SchemaName,
					Table:  schemalog.TableName,
					Columns: []wal.Column{
						{Name: "schema_name", Value: 1},
					},
				},
				CommitPosition: testCommitPosition,
			},

			wantMsgs: []*msg{},
			wantErr:  errors.New("kafka batch writer: understanding event: schema_log schema_name received is not a string: int"),
		},
		{
			name: "panic recovery - schema_name not found",
			walEvent: &wal.Event{
				Data: &wal.Data{
					Action: "I",
					LSN:    testLSNStr,
					Schema: schemalog.SchemaName,
					Table:  schemalog.TableName,
				},
				CommitPosition: testCommitPosition,
			},

			wantMsgs: []*msg{},
			wantErr:  errors.New("kafka batch writer: understanding event: schema_log schema_name not found in columns"),
		},
		{
			name:     "error - acquiring semaphore",
			walEvent: testWalEvent,
			semaphore: &syncmocks.WeightedSemaphore{
				TryAcquireFn: func(int64) bool { return false },
				AcquireFn:    func(_ context.Context, i int64) error { return errTest },
			},

			wantMsgs: []*msg{},
			wantErr:  errTest,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			writer := &BatchWriter{
				logger:         loglib.NewNoopLogger(),
				msgChan:        make(chan *msg),
				maxBatchBytes:  100,
				queueBytesSema: semaphore.NewWeighted(defaultMaxQueueBytes),
				serialiser:     mockMarshaler,
			}

			if tc.semaphore != nil {
				writer.queueBytesSema = tc.semaphore
			}

			if tc.eventSerialiser != nil {
				writer.serialiser = tc.eventSerialiser
			}

			go func() {
				defer close(writer.msgChan)
				err := writer.ProcessWALEvent(context.Background(), tc.walEvent)
				if !errors.Is(err, tc.wantErr) {
					require.Equal(t, err.Error(), tc.wantErr.Error())
				}
			}()

			msgs := []*msg{}
			for msg := range writer.msgChan {
				msgs = append(msgs, msg)
				writer.queueBytesSema.Release(int64(msg.size()))
			}
			require.Equal(t, tc.wantMsgs, msgs)
		})
	}
}

func TestBatchKafkaWriter_SendThread(t *testing.T) {
	t.Parallel()

	testCommitPosition := wal.CommitPosition(testLSNStr)
	testBytes := []byte("test")
	testKafkaMsg := &msg{
		msg: kafka.Message{
			Key:   []byte(testSchema),
			Value: testBytes,
		},
		pos: testCommitPosition,
	}

	tests := []struct {
		name             string
		writerValidation func(i uint64, doneChan chan struct{}, msgs ...kafka.Message) error
		msgs             []*msg
		semaphore        *syncmocks.WeightedSemaphore

		wantWriteCalls   uint64
		wantReleaseCalls uint64
		wantErr          error
	}{
		{
			name: "ok",
			msgs: []*msg{testKafkaMsg},
			writerValidation: func(i uint64, doneChan chan struct{}, msgs ...kafka.Message) error {
				defer func() {
					doneChan <- struct{}{}
				}()
				if i == 1 {
					require.Equal(t, 1, len(msgs))
					require.Equal(t, testBytes, msgs[0].Value)
					require.Equal(t, testSchema, string(msgs[0].Key))
					return nil
				}
				return fmt.Errorf("unexpected write call: %d", i)
			},
			semaphore: &syncmocks.WeightedSemaphore{
				ReleaseFn: func(_ uint64, size int64) {
					require.Equal(t, len(testBytes), int(size))
				},
			},

			wantWriteCalls:   1,
			wantReleaseCalls: 1,
			wantErr:          context.Canceled,
		},
		{
			name: "ok - max batch bytes reached, trigger send",
			msgs: []*msg{
				{
					msg: kafka.Message{
						Key:   []byte(testSchema),
						Value: []byte(strings.Repeat("a", 51)),
					},
					pos: testCommitPosition,
				},
				{
					msg: kafka.Message{
						Key:   []byte(testSchema),
						Value: []byte(strings.Repeat("b", 50)),
					},
					pos: testCommitPosition,
				},
				{
					msg: kafka.Message{
						Key:   []byte(testSchema),
						Value: []byte(strings.Repeat("c", 10)),
					},
					pos: testCommitPosition,
				},
			},
			writerValidation: func(i uint64, doneChan chan struct{}, msgs ...kafka.Message) error {
				defer func() {
					if i == 2 {
						doneChan <- struct{}{}
					}
				}()
				switch i {
				case 1:
					require.Equal(t, 1, len(msgs))
					require.Equal(t, []byte(strings.Repeat("a", 51)), msgs[0].Value)
					require.Equal(t, testSchema, string(msgs[0].Key))
					return nil
				case 2:
					require.Equal(t, 2, len(msgs))
					require.Equal(t, []byte(strings.Repeat("b", 50)), msgs[0].Value)
					require.Equal(t, testSchema, string(msgs[0].Key))
					require.Equal(t, []byte(strings.Repeat("c", 10)), msgs[1].Value)
					require.Equal(t, testSchema, string(msgs[1].Key))
				}
				return nil
			},
			semaphore: &syncmocks.WeightedSemaphore{
				ReleaseFn: func(i uint64, size int64) {
					switch i {
					case 1:
						require.Equal(t, int64(51), size)
					case 2:
						require.Equal(t, int64(60), size)
					default:
						require.Fail(t, fmt.Sprintf("unexpected call to release: %d", i))
					}
				},
			},

			wantWriteCalls:   2,
			wantReleaseCalls: 2,
			wantErr:          context.Canceled,
		},
		{
			name: "error - writing messages",
			msgs: []*msg{testKafkaMsg},
			writerValidation: func(i uint64, doneChan chan struct{}, msgs ...kafka.Message) error {
				defer func() {
					doneChan <- struct{}{}
				}()
				return errTest
			},
			semaphore: &syncmocks.WeightedSemaphore{
				ReleaseFn: func(_ uint64, size int64) {
					require.Equal(t, len(testBytes), int(size))
				},
			},

			wantWriteCalls:   1,
			wantReleaseCalls: 1,
			wantErr:          errTest,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doneChan := make(chan struct{})
			defer close(doneChan)
			mockWriter := &kafkamocks.Writer{
				WriteMessagesFn: func(ctx context.Context, i uint64, msgs ...kafka.Message) error {
					return tc.writerValidation(i, doneChan, msgs...)
				},
			}

			writer := &BatchWriter{
				logger:         loglib.NewNoopLogger(),
				writer:         mockWriter,
				msgChan:        make(chan *msg),
				maxBatchBytes:  100,
				maxBatchSize:   10,
				queueBytesSema: tc.semaphore,
				sendFrequency:  time.Second,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			wg := sync.WaitGroup{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := writer.Send(ctx)
				require.ErrorIs(t, err, tc.wantErr)
				require.Equal(t, tc.wantWriteCalls, mockWriter.GetWriteCalls())
				require.Equal(t, tc.wantReleaseCalls, tc.semaphore.GetReleaseCalls())
			}()

			for _, msg := range tc.msgs {
				writer.msgChan <- msg
			}
			// make sure the test doesn't block indefinitely if something goes
			// wrong
			for {
				select {
				case <-doneChan:
					if errors.Is(tc.wantErr, context.Canceled) {
						cancel()
					}
					wg.Wait()
					return
				case <-ctx.Done():
					wg.Wait()
					return
				}
			}
		})
	}
}

func TestBatchKafkaWriter_sendBatch(t *testing.T) {
	t.Parallel()

	testCommitPosition := wal.CommitPosition(testLSNStr)
	testBytes := []byte("test")
	testBatch := &msgBatch{
		msgs: []kafka.Message{
			{
				Key:   []byte(testSchema),
				Value: testBytes,
			},
		},
		positions: []wal.CommitPosition{testCommitPosition},
	}

	tests := []struct {
		name       string
		writer     *kafkamocks.Writer
		checkpoint checkpointer.Checkpoint
		batch      *msgBatch

		wantErr error
	}{
		{
			name: "ok",
			writer: &kafkamocks.Writer{
				WriteMessagesFn: func(ctx context.Context, i uint64, msgs ...kafka.Message) error {
					require.Equal(t, 1, len(msgs))
					require.Equal(t, testBytes, msgs[0].Value)
					require.Equal(t, testSchema, string(msgs[0].Key))
					return nil
				},
			},
			checkpoint: func(_ context.Context, commitPos []wal.CommitPosition) error {
				require.Equal(t, 1, len(commitPos))
				require.Equal(t, testCommitPosition, commitPos[0])
				return nil
			},
			batch: testBatch,

			wantErr: nil,
		},
		{
			name: "ok - empty batch",
			writer: &kafkamocks.Writer{
				WriteMessagesFn: func(ctx context.Context, i uint64, msgs ...kafka.Message) error {
					return errors.New("WriteMessagesFn: should not be called")
				},
			},
			checkpoint: func(_ context.Context, commitPos []wal.CommitPosition) error {
				return errors.New("checkpoint: should not be called")
			},
			batch: &msgBatch{
				msgs: []kafka.Message{},
			},

			wantErr: nil,
		},
		{
			name: "ok - error checkpointing",
			writer: &kafkamocks.Writer{
				WriteMessagesFn: func(ctx context.Context, i uint64, msgs ...kafka.Message) error {
					require.Equal(t, 1, len(msgs))
					require.Equal(t, testBytes, msgs[0].Value)
					require.Equal(t, testSchema, string(msgs[0].Key))
					return nil
				},
			},
			checkpoint: func(_ context.Context, commitPos []wal.CommitPosition) error {
				return errTest
			},
			batch: testBatch,

			wantErr: nil,
		},
		{
			name: "error - writing messages",
			writer: &kafkamocks.Writer{
				WriteMessagesFn: func(ctx context.Context, i uint64, msgs ...kafka.Message) error {
					return errTest
				},
			},
			checkpoint: func(_ context.Context, commitPos []wal.CommitPosition) error {
				return errors.New("checkpoint: should not be called")
			},
			batch: testBatch,

			wantErr: errTest,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			writer := &BatchWriter{
				logger:       loglib.NewNoopLogger(),
				writer:       tc.writer,
				checkpointer: tc.checkpoint,
			}

			err := writer.sendBatch(context.Background(), tc.batch)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}
