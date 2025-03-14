package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephantine"
	"golang.org/x/sync/errgroup"
)

type ChannelSubscription interface {
	// ChannelName to listen to.
	ChannelName() string
	// NotifyWithPayload notifies local consumers of the message.
	NotifyWithPayload(data []byte) error
}

// Publish a JSON message on a pubsub channel.
func Publish(
	ctx context.Context, db DBExec,
	channel string, message any,
) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal message to JSON: %w", err)
	}

	_, err = db.Exec(ctx,
		"SELECT pg_notify($1::text, $2::text)",
		channel, string(data),
	)
	if err != nil {
		return fmt.Errorf("notify postgres channel: %w", err)
	}

	return nil
}

// Subscribe opens a connection to the database and subscribes to the provided
// channels. Blocks until the context is cancelled.
func Subscribe(
	ctx context.Context,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	channels ...ChannelSubscription,
) {
	for {
		err := runListener(ctx, logger, pool, channels)
		if errors.Is(err, context.Canceled) {
			return
		} else if err != nil {
			logger.ErrorContext(
				ctx, "failed to run notification listener",
				elephantine.LogKeyError, err,
			)
		}

		time.Sleep(5 * time.Second)
	}
}

func runListener(
	ctx context.Context,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	channels []ChannelSubscription,
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection from pool: %w", err)
	}

	pConn := conn.Hijack()

	defer func() {
		err := pConn.Close(ctx)
		if err != nil {
			logger.ErrorContext(ctx,
				"failed to close PG listen connection",
				elephantine.LogKeyError, err)
		}
	}()

	lookup := make(map[string]ChannelSubscription, len(channels))

	for _, channel := range channels {
		ident := pgx.Identifier{channel.ChannelName()}

		_, err := pConn.Exec(ctx, "LISTEN "+ident.Sanitize())
		if err != nil {
			return fmt.Errorf("failed to start listening to %q: %w",
				channel, err)
		}

		lookup[channel.ChannelName()] = channel
	}

	received := make(chan *pgconn.Notification)
	grp, gCtx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		for {
			notification, err := pConn.WaitForNotification(gCtx)
			if err != nil {
				return fmt.Errorf(
					"error while waiting for notification: %w", err)
			}

			received <- notification
		}
	})

	grp.Go(func() error {
		for {
			var notification *pgconn.Notification

			select {
			case <-ctx.Done():
				return ctx.Err()
			case notification = <-received:
			}

			channel, ok := lookup[notification.Channel]
			if !ok {
				continue
			}

			err := channel.NotifyWithPayload([]byte(notification.Payload))
			if err != nil {
				logger.Warn("invalid payload for PG notification",
					"err", err,
					"channel", notification.Channel)
			}
		}
	})

	err = grp.Wait()
	if err != nil {
		return err //nolint:wrapcheck
	}

	return nil
}

type FanOut[T any] struct {
	channel   string
	m         sync.RWMutex
	listeners map[chan T]func(v T) bool
}

func NewFanOut[T any](channel string) *FanOut[T] {
	return &FanOut[T]{
		channel:   channel,
		listeners: make(map[chan T]func(v T) bool),
	}
}

// ListenAll listens for notifications until the context is cancelled.
func (f *FanOut[T]) ListenAll(ctx context.Context, l chan T) {
	f.Listen(ctx, l, func(v T) bool {
		return true
	})
}

// Listen for notifications until the context is cancelled. The test function is
// used to filter out events before they are posted to the channel.
func (f *FanOut[T]) Listen(ctx context.Context, l chan T, test func(v T) bool) {
	f.m.Lock()
	f.listeners[l] = test
	f.m.Unlock()

	<-ctx.Done()

	f.m.Lock()
	delete(f.listeners, l)
	f.m.Unlock()
}

// Implements ChannelSubscription.
func (f *FanOut[T]) ChannelName() string {
	return f.channel
}

// Implements ChannelSubscription.
func (f *FanOut[T]) NotifyWithPayload(data []byte) error {
	var e T

	err := json.Unmarshal(data, &e)
	if err != nil {
		return fmt.Errorf("invalid JSON payload: %w", err)
	}

	f.Notify(e)

	return nil
}

// Notify local consumers of a message.
func (f *FanOut[T]) Notify(msg T) {
	f.m.RLock()
	defer f.m.RUnlock()

	for listener, test := range f.listeners {
		if !test(msg) {
			continue
		}

		select {
		case listener <- msg:
		default:
		}
	}
}

// Publish a message to the channel.
func (f *FanOut[T]) Publish(ctx context.Context, db DBExec, msg T) error {
	return Publish(ctx, db, f.channel, msg)
}
