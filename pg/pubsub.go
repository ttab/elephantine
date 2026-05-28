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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
	"golang.org/x/sync/errgroup"
)

// PingChannel is the PostgreSQL NOTIFY channel used for listener health
// checking. The Subscriber sends periodic pings on this channel and uses the
// received notifications to verify that the LISTEN connection is still alive.
const PingChannel = "listener_ping"

var (
	errPingTimeout = errors.New("listener ping timeout")
	errBounce      = errors.New("listener bounce requested")
)

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithPingDB sets a separate database connection for sending pings. This is
// useful when the listen pool goes through PgBouncer in transaction mode, where
// LISTEN is not supported, but the ping sender needs a regular connection. Set
// to nil to disable the built-in ping sender entirely (useful when an external
// process sends pings).
func WithPingDB(db DBExec) SubscriberOption {
	return func(s *Subscriber) {
		s.pingDB = db
		s.pingDBSet = true
	}
}

// WithPingInterval sets how often the subscriber sends a ping notification.
// Defaults to 5 minutes.
func WithPingInterval(d time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		s.pingInterval = d
	}
}

// WithPingGrace sets how long the subscriber waits for a ping before declaring
// the connection dead. Must be longer than the ping interval. Defaults to 7
// minutes.
func WithPingGrace(d time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		s.pingGrace = d
	}
}

// WithOnReconnect sets a callback that is called before the initial listen and
// after each reconnect. This is useful for reloading state that may have changed
// while disconnected.
func WithOnReconnect(fn func(ctx context.Context) error) SubscriberOption {
	return func(s *Subscriber) {
		s.onReconnect = fn
	}
}

// Subscriber manages a PostgreSQL LISTEN connection with ping-based health
// checking. It detects silently dead connections (TCP drops, PgBouncer timeouts,
// network partitions) by sending periodic pings and using deadlines on the
// notification wait.
type Subscriber struct {
	logger       *slog.Logger
	listenPool   *pgxpool.Pool
	pingDB       DBExec
	pingDBSet    bool
	pingInterval time.Duration
	pingGrace    time.Duration
	channels     []ChannelSubscription
	onReconnect  func(ctx context.Context) error
	// bounceCh receives requests to tear down the current LISTEN connection
	// and reconnect. Buffered to depth 1 so multiple concurrent Bounce calls
	// coalesce into one pending signal; runListenerWithPing drains it on
	// entry so signals queued during a previous outage don't immediately
	// trip the fresh connection.
	bounceCh chan struct{}
}

// NewSubscriber creates a new Subscriber that listens on the given channels. By
// default it uses the provided pool for both listening and sending pings.
func NewSubscriber(
	logger *slog.Logger,
	pool *pgxpool.Pool,
	channels []ChannelSubscription,
	opts ...SubscriberOption,
) *Subscriber {
	s := &Subscriber{
		logger:       logger,
		listenPool:   pool,
		pingDB:       pool,
		pingInterval: 5 * time.Minute,
		pingGrace:    7 * time.Minute,
		channels:     channels,
		bounceCh:     make(chan struct{}, 1),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Bounce signals the listen loop to tear down the current PostgreSQL LISTEN
// connection and reconnect on the next iteration. The call is non-blocking;
// concurrent Bounce invocations coalesce into a single pending signal, and the
// listen loop drains any pending signal at the start of each connection so a
// signal queued during a previous outage does not immediately tear down a
// fresh connection.
//
// Bounce is intended for forcing a reconnect when an external signal (e.g. a
// RecoveryTracker noticing that polling is keeping a consumer afloat without
// notifications) suggests the LISTEN connection is silently broken even
// though the ping path appears healthy.
func (s *Subscriber) Bounce() {
	select {
	case s.bounceCh <- struct{}{}:
	default:
	}
}

// SendListenerPing sends a ping notification on the PingChannel. This can be
// used by external processes to keep the listener alive when the built-in ping
// sender is disabled.
func SendListenerPing(ctx context.Context, db DBExec) error {
	_, err := db.Exec(ctx,
		"SELECT pg_notify($1::text, '')",
		PingChannel,
	)
	if err != nil {
		return fmt.Errorf("send listener ping: %w", err)
	}

	return nil
}

// Run starts the subscriber and blocks until the context is cancelled or a
// fatal error occurs. It automatically reconnects on ping timeouts.
func (s *Subscriber) Run(ctx context.Context) error {
	grp, gCtx := errgroup.WithContext(ctx)

	if !s.pingDBSet || s.pingDB != nil {
		grp.Go(func() error {
			s.runPingSender(gCtx)

			return nil
		})
	}

	grp.Go(func() error {
		return s.runListenLoop(gCtx)
	})

	err := grp.Wait()
	if err != nil {
		return err //nolint:wrapcheck
	}

	return nil
}

func (s *Subscriber) runPingSender(ctx context.Context) {
	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := SendListenerPing(ctx, s.pingDB)
			if err != nil {
				s.logger.ErrorContext(ctx,
					"send listener ping",
					elephantine.LogKeyError, err,
				)
			}
		}
	}
}

func (s *Subscriber) runListenLoop(ctx context.Context) error {
	for {
		err := s.runListenerWithPing(ctx)

		switch {
		case errors.Is(err, context.Canceled):
			return ctx.Err() //nolint:wrapcheck
		case errors.Is(err, errPingTimeout):
			s.logger.WarnContext(ctx,
				"listener ping timeout, reconnecting",
			)
		case errors.Is(err, errBounce):
			s.logger.WarnContext(ctx,
				"listener bounce requested, reconnecting",
			)
		case err != nil:
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *Subscriber) runListenerWithPing(ctx context.Context) (outErr error) {
	// Drain any stale bounce signal queued during a previous outage so that
	// the fresh connection we're about to set up is not immediately torn
	// down. With multiple consumers, several Bounce calls can race during a
	// single outage; the buffered channel coalesces them and this drain
	// discards the leftover.
	select {
	case <-s.bounceCh:
	default:
	}

	if s.onReconnect != nil {
		err := s.onReconnect(ctx)
		if err != nil {
			return fmt.Errorf("on reconnect: %w", err)
		}
	}

	conn, err := s.listenPool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection from pool: %w", err)
	}

	pConn := conn.Hijack()

	defer func() {
		err := pConn.Close(ctx)
		if err != nil {
			s.logger.ErrorContext(ctx,
				"close PG listen connection",
				elephantine.LogKeyError, err,
			)
		}
	}()

	lookup := make(map[string]ChannelSubscription, len(s.channels))

	for _, channel := range s.channels {
		ident := pgx.Identifier{channel.ChannelName()}

		_, err := pConn.Exec(ctx, "LISTEN "+ident.Sanitize())
		if err != nil {
			return fmt.Errorf("start listening to %q: %w",
				channel.ChannelName(), err)
		}

		lookup[channel.ChannelName()] = channel
	}

	// Also listen for pings.
	pingIdent := pgx.Identifier{PingChannel}

	_, err = pConn.Exec(ctx, "LISTEN "+pingIdent.Sanitize())
	if err != nil {
		return fmt.Errorf("start listening to ping channel: %w", err)
	}

	// bounceCtx is a child of ctx that we cancel as soon as a bounce signal
	// arrives. The waiter goroutine below watches bounceCh for the lifetime
	// of this listen attempt; it exits when bounceCtx is done (either
	// because we bounced, the parent was cancelled, or this function
	// returned).
	bounceCtx, cancelBounce := context.WithCancel(ctx)
	defer cancelBounce()

	go func() {
		select {
		case <-s.bounceCh:
			cancelBounce()
		case <-bounceCtx.Done():
		}
	}()

	lastPing := time.Now()

	for {
		deadline := lastPing.Add(s.pingGrace)

		waitCtx, waitCancel := context.WithDeadline(bounceCtx, deadline)

		notification, err := pConn.WaitForNotification(waitCtx)

		waitCancel()

		if err != nil {
			// A bounce signal cancels bounceCtx while the parent ctx
			// is still healthy; surface it so the outer loop can
			// reconnect.
			if bounceCtx.Err() != nil && ctx.Err() == nil {
				return errBounce
			}

			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return errPingTimeout
			}

			return fmt.Errorf("wait for notification: %w", err)
		}

		if notification.Channel == PingChannel {
			lastPing = time.Now()

			continue
		}

		channel, ok := lookup[notification.Channel]
		if !ok {
			continue
		}

		err = channel.NotifyWithPayload([]byte(notification.Payload))
		if err != nil {
			s.logger.WarnContext(ctx, "invalid payload for PG notification",
				elephantine.LogKeyError, err,
				"channel", notification.Channel,
			)
		}
	}
}

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
//
// Deprecated: use NewSubscriber and Subscriber.Run instead, which adds
// ping-based health checking to detect dead connections.
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
			case <-gCtx.Done():
				return gCtx.Err()
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

// OverflowPolicy controls what happens when a FanOut listener's channel is
// full.
type OverflowPolicy int

const (
	// OverflowDrop silently drops the message. This is the default and
	// preserves the existing behavior.
	OverflowDrop OverflowPolicy = iota
	// OverflowError causes Notify to return an error when a listener's
	// channel is full.
	OverflowError
)

// ErrListenerOverflow is returned by Notify when a listener's channel is full
// and the overflow policy is OverflowError.
var ErrListenerOverflow = errors.New("listener channel full")

// FanOutOption configures a FanOut.
type FanOutOption func(*fanOutConfig)

type fanOutConfig struct {
	overflowPolicy OverflowPolicy
}

// WithOverflowPolicy sets the overflow policy for the FanOut.
func WithOverflowPolicy(p OverflowPolicy) FanOutOption {
	return func(c *fanOutConfig) {
		c.overflowPolicy = p
	}
}

type FanOut[T any] struct {
	channel        string
	overflowPolicy OverflowPolicy
	m              sync.RWMutex
	listeners      map[chan T]func(v T) bool
	// recovery is non-nil once EnableRecovery has been called. Reads and
	// writes are guarded by m so EnableRecovery is safe to call after
	// listeners have started, though that is rarely useful.
	recovery *recoveryTracker
}

func NewFanOut[T any](channel string, opts ...FanOutOption) *FanOut[T] {
	var cfg fanOutConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	return &FanOut[T]{
		channel:        channel,
		overflowPolicy: cfg.overflowPolicy,
		listeners:      make(map[chan T]func(v T) bool),
	}
}

// EnableRecovery wires this FanOut into the recovery pattern documented in
// elephantine's docs/fanout-recovery.md: callers report fallback-poll
// findings via Polled, the FanOut resets the streak whenever it dispatches a
// wire-side notification (NotifyWithPayload), and bounce is invoked once the
// poll-saved streak crosses the configured threshold (default 5). Pass
// Subscriber.Bounce for bounce so the recovery loop closes back onto the
// listener.
//
// EnableRecovery registers a per-channel poll-saved counter and streak gauge
// through metrics, using the channel name (sanitized) as the metric name
// suffix. Registration errors are returned and also recorded on the metrics
// helper.
//
// Calling EnableRecovery more than once replaces the previous wiring; that
// is rarely useful but legal.
func (f *FanOut[T]) EnableRecovery(
	metrics *elephantine.MetricsHelper,
	bounce func(),
	opts ...FanOutRecoveryOption,
) error {
	cfg := fanOutRecoveryConfig{threshold: defaultBounceThreshold}

	for _, opt := range opts {
		opt(&cfg)
	}

	suffix := sanitizeMetricSegment(f.channel)

	var (
		pollCounter prometheus.Counter
		streakGauge prometheus.Gauge
	)

	metrics.Counter(&pollCounter, prometheus.CounterOpts{
		Name: "pg_fanout_" + suffix + "_poll_saved_total",
		Help: "Items the fallback poll for FanOut channel " + f.channel +
			" relayed before a notification arrived.",
	})

	metrics.Gauge(&streakGauge, prometheus.GaugeOpts{
		Name: "pg_fanout_" + suffix + "_poll_saved_streak",
		Help: "Consecutive non-empty fallback-poll drains for FanOut " +
			"channel " + f.channel + " without an intervening " +
			"notification. Counts calls, not items, so a busy " +
			"publisher cannot trip the bounce on a single mistimed " +
			"poll.",
	})

	err := metrics.Err()
	if err != nil {
		return fmt.Errorf("register FanOut recovery metrics: %w", err)
	}

	tracker := newRecoveryTracker(bounce, cfg.threshold, pollCounter, streakGauge)

	f.m.Lock()
	f.recovery = tracker
	f.m.Unlock()

	return nil
}

// Polled reports that a fallback-poll iteration discovered `found` items of
// work. With recovery enabled, a call with found > 0 advances the consecutive
// non-empty-poll streak by one (regardless of how many items were drained,
// so a backlog-clearing poll does not trip the threshold by itself) and adds
// `found` to the per-item counter. Once the streak crosses the configured
// threshold the bounce callback fires and the streak resets. With recovery
// not enabled, Polled is a no-op; consumers may call it unconditionally. A
// call with found <= 0 is always a no-op.
func (f *FanOut[T]) Polled(found int) {
	f.m.RLock()
	rec := f.recovery
	f.m.RUnlock()

	if rec == nil {
		return
	}

	rec.polled(found)
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

// Implements ChannelSubscription. The wire-side delivery path: invoked by the
// Subscriber when a Postgres NOTIFY arrives on this channel. Successful
// dispatch resets the recovery streak (if enabled), signalling that the
// LISTEN connection is healthy.
func (f *FanOut[T]) NotifyWithPayload(data []byte) error {
	var e T

	err := json.Unmarshal(data, &e)
	if err != nil {
		return fmt.Errorf("invalid JSON payload: %w", err)
	}

	err = f.Notify(e)
	if err != nil {
		return fmt.Errorf("notify listeners: %w", err)
	}

	f.m.RLock()
	rec := f.recovery
	f.m.RUnlock()

	if rec != nil {
		rec.notified()
	}

	return nil
}

// Notify local consumers of a message.
func (f *FanOut[T]) Notify(msg T) error {
	f.m.RLock()
	defer f.m.RUnlock()

	for listener, test := range f.listeners {
		if !test(msg) {
			continue
		}

		select {
		case listener <- msg:
		default:
			if f.overflowPolicy == OverflowError {
				return ErrListenerOverflow
			}
		}
	}

	return nil
}

// Publish a message to the channel.
func (f *FanOut[T]) Publish(ctx context.Context, db DBExec, msg T) error {
	return Publish(ctx, db, f.channel, msg)
}
