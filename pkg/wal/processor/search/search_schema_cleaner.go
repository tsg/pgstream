// SPDX-License-Identifier: Apache-2.0

package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xataio/pgstream/internal/backoff"

	"github.com/rs/zerolog/log"
)

type cleaner interface {
	deleteSchema(context.Context, string) error
	start(context.Context)
	stop()
}

type store interface {
	DeleteSchema(ctx context.Context, schemaName string) error
}

type schemaCleaner struct {
	deleteSchemaQueue   chan string
	store               store
	backoffProvider     backoff.Provider
	registrationTimeout time.Duration
}

const (
	maxDeleteQueueSize         = 5000
	defaultRegistrationTimeout = 5 * time.Second
)

var errRegistrationTimeout = errors.New("timeout registering schema for clean up")

func newSchemaCleaner(cfg *backoff.Config, store store) *schemaCleaner {
	return &schemaCleaner{
		deleteSchemaQueue:   make(chan string, maxDeleteQueueSize),
		store:               store,
		registrationTimeout: defaultRegistrationTimeout,
		backoffProvider: func(ctx context.Context) backoff.Backoff {
			return backoff.NewExponentialBackoff(ctx, cfg)
		},
	}
}

// deleteSchema writes a delete schema item to the delete queue. Times out and returns an error after 5 seconds.
func (sc *schemaCleaner) deleteSchema(_ context.Context, schemaName string) error {
	select {
	case sc.deleteSchemaQueue <- schemaName:
		return nil
	case <-time.After(sc.registrationTimeout):
		return errRegistrationTimeout
	}
}

// start will continuously process schema items from the local delete queue
func (sc *schemaCleaner) start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case schema := <-sc.deleteSchemaQueue:
			bo := sc.backoffProvider(ctx)
			err := bo.RetryNotify(
				func() error {
					return getRetryError(sc.store.DeleteSchema(ctx, schema))
				},
				func(err error, duration time.Duration) {
					log.Ctx(ctx).Warn().Err(err).
						Dur("backoff", duration).
						Str("schema", schema).
						Msg("search schema cleaner: delete schema retry failed")
				})
			if err != nil {
				log.Ctx(ctx).Error().Err(err).
					Str("schema", schema).
					Msg("search schema cleaner: delete schema")
			}
		}
	}
}

// stop will stop the processing of delete items from the queue and release
// internal resources
func (sc schemaCleaner) stop() {
	close(sc.deleteSchemaQueue)
}

// getRetryError returns a backoff permanent error if the given error is not
// retryable
func getRetryError(err error) error {
	if err != nil {
		if errors.Is(err, ErrRetriable) {
			return err
		}
		return fmt.Errorf("%w: %w", err, backoff.ErrPermanent)
	}
	return nil
}
