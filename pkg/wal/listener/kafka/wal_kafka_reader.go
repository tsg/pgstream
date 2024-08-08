// SPDX-License-Identifier: Apache-2.0

package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/xataio/pgstream/pkg/kafka"
	loglib "github.com/xataio/pgstream/pkg/log"
	"github.com/xataio/pgstream/pkg/wal"
)

// Reader is a kafka reader that listens to wal events.
type Reader struct {
	reader       kafkaReader
	unmarshaler  func([]byte, any) error
	logger       loglib.Logger
	offsetParser kafka.OffsetParser

	// processRecord is called for a new record.
	processRecord payloadProcessor
}

type ReaderConfig struct {
	Kafka kafka.ReaderConfig
}

type kafkaReader interface {
	FetchMessage(context.Context) (*kafka.Message, error)
	Close() error
}

type payloadProcessor func(context.Context, *wal.Event) error

type Option func(*Reader)

// NewReader returns a kafka reader that listens to wal events and calls the
// processor on input.
func NewReader(config ReaderConfig, processRecord payloadProcessor, opts ...Option) (*Reader, error) {
	r := &Reader{
		logger:        loglib.NewNoopLogger(),
		processRecord: processRecord,
		unmarshaler:   json.Unmarshal,
		offsetParser:  kafka.NewOffsetParser(),
	}

	for _, opt := range opts {
		opt(r)
	}

	var err error
	r.reader, err = kafka.NewReader(config.Kafka, r.logger)
	if err != nil {
		return nil, err
	}

	return r, nil
}

func WithLogger(logger loglib.Logger) Option {
	return func(r *Reader) {
		r.logger = loglib.NewLogger(logger).WithFields(loglib.Fields{
			loglib.ServiceField: "wal_kafka_reader",
		})
	}
}

func (r *Reader) Listen(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			msg, err := r.reader.FetchMessage(ctx)
			if err != nil {
				return fmt.Errorf("reading from kafka: %w", err)
			}

			r.logger.Trace("received", loglib.Fields{
				"topic":     msg.Topic,
				"partition": msg.Partition,
				"offset":    msg.Offset,
				"key":       msg.Key,
				"wal_data":  msg.Value,
			})

			offset := &kafka.Offset{
				Topic:     msg.Topic,
				Partition: msg.Partition,
				Offset:    msg.Offset,
			}

			event := &wal.Event{
				CommitPosition: wal.CommitPosition(r.offsetParser.ToString(offset)),
			}
			event.Data = &wal.Data{}
			if err := r.unmarshaler(msg.Value, event.Data); err != nil {
				return fmt.Errorf("error unmarshaling message value into wal data: %w", err)
			}

			if err = r.processRecord(ctx, event); err != nil {
				if errors.Is(err, context.Canceled) {
					return fmt.Errorf("canceled: %w", err)
				}

				r.logger.Error(err, "processing kafka msg", loglib.Fields{
					"severity": "DATALOSS",
					"wal_data": msg.Value,
				})
			}
		}
	}
}

func (r *Reader) Close() error {
	// Cleanly closing the connection to Kafka is important
	// in order for the consumer's partitions to be re-allocated
	// quickly.
	if err := r.reader.Close(); err != nil {
		r.logger.Error(err, "error closing connection to kafka", loglib.Fields{
			"stack_trace": debug.Stack(),
		})
		return err
	}
	return nil
}
