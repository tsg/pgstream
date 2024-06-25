// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	httplib "github.com/xataio/pgstream/internal/http"
	loglib "github.com/xataio/pgstream/pkg/log"
	"github.com/xataio/pgstream/pkg/wal/processor/webhook"
)

type SubscriptionServer struct {
	server  httplib.Server
	logger  loglib.Logger
	store   webhook.SubscriptionStore
	address string
}

type Option func(*SubscriptionServer)

func New(cfg *Config, store webhook.SubscriptionStore, opts ...Option) *SubscriptionServer {
	s := &SubscriptionServer{
		address: cfg.address(),
		store:   store,
		logger:  loglib.NewNoopLogger(),
	}

	e := echo.New()
	e.Server.ReadTimeout = cfg.readTimeout()
	e.Server.WriteTimeout = cfg.writeTimeout()

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.POST("/webhooks/subscribe", s.subscribe)
	e.POST("/webhooks/unsubscribe", s.unsubscribe)

	s.server = e

	for _, opt := range opts {
		opt(s)
	}

	return s
}

func WithLogger(l loglib.Logger) Option {
	return func(s *SubscriptionServer) {
		s.logger = loglib.NewLogger(l).WithFields(loglib.Fields{
			loglib.ServiceField: "webhook_subscription_server",
		})
	}
}

// Start will start the subscription server. This call is blocking.
func (s *SubscriptionServer) Start() error {
	s.logger.Info(fmt.Sprintf("subscription server listening on: %s...", s.address))
	return s.server.Start(s.address)
}

func (s *SubscriptionServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *SubscriptionServer) subscribe(c echo.Context) error {
	if c.Request().Method != http.MethodPost {
		return c.JSON(http.StatusMethodNotAllowed, nil)
	}

	s.logger.Trace("request received on /subscribe endpoint")

	subscription := &webhook.Subscription{}
	if err := c.Bind(subscription); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	ctx := c.Request().Context()
	if err := s.store.CreateSubscription(ctx, subscription); err != nil {
		return c.JSON(http.StatusServiceUnavailable, err)
	}

	return c.JSON(http.StatusCreated, nil)
}

func (s *SubscriptionServer) unsubscribe(c echo.Context) error {
	if c.Request().Method != http.MethodPost {
		return c.JSON(http.StatusMethodNotAllowed, nil)
	}

	s.logger.Trace("request received on /unsubscribe endpoint")
	subscription := &webhook.Subscription{}
	if err := c.Bind(subscription); err != nil {
		return c.JSON(http.StatusBadRequest, err)
	}

	ctx := c.Request().Context()
	if err := s.store.DeleteSubscription(ctx, subscription); err != nil {
		return c.JSON(http.StatusServiceUnavailable, err)
	}

	return c.JSON(http.StatusOK, nil)
}