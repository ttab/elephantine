# FanOut recovery

The `pg.Subscriber` + `pg.FanOut[T]` pair gives a process a single LISTEN
connection that distributes Postgres notifications to multiple in-process
consumers. The `Subscriber` already detects a silently dead listen
connection via periodic pings (see `WithPingInterval` / `WithPingGrace`),
but the ping path can stay healthy while application notifications stop
flowing — for example after a PgBouncer reset that swallowed the LISTEN
re-issue, or when a deploy script restarted only one side of the
publish/subscribe pair.

`FanOut.EnableRecovery` + `Subscriber.Bounce` close that gap. A consumer
that combines a real-time notification path with a slower fallback poll
can observe when polling is the only thing keeping it caught up, surface
that state to operators as a Prometheus metric, and have the FanOut force
the `Subscriber` to tear down its LISTEN connection and reconnect once
the divergence crosses a threshold.

## The signal

A consumer of a FanOut typically has two ways to learn about new work:

1. A wire-side notification arrives via the FanOut. The FanOut delivers
   it to listeners. The consumer drains its underlying state.
2. A periodic fallback poll runs (every few seconds) and queries the
   underlying state directly. If it finds work, the LISTEN path failed to
   deliver — or the publisher hasn't fired NOTIFY yet.

Under normal conditions notifications race ahead of the poll: polls find
nothing, the FanOut delivers each event, and the consumer drains via the
notification path. When notifications stop flowing while writes continue,
the poll starts finding work consistently. That state — "polls keeping
the consumer afloat without notifications" — is the symptom of a broken
subscription, and is what the recovery wiring measures.

> A single poll discovering work just before a notification arrives is
> normal racing, not a fault. The threshold absorbs that background
> noise.

## How it works

The FanOut owns the recovery state once `EnableRecovery` has been called:

- **`FanOut.Polled(found int)`** is the consumer-facing report. Call it
  after each fallback-poll iteration with the number of items the
  iteration found. A `found == 0` call is a no-op; the FanOut only cares
  whether each call did or did not find work.
- **The streak counts calls, not items.** Each non-empty poll advances
  the streak by exactly one, no matter how much backlog it cleared.
  This way a busy publisher whose single mistimed poll catches up 1000
  items does not trip the bounce — the threshold only fires when polls
  *keep* finding work without notifications in between.
- **The wire-side reset is automatic.** Whenever `FanOut.NotifyWithPayload`
  dispatches a real PG NOTIFY to listeners, it resets the streak. The
  consumer does not need to do anything to participate.
- **`Subscriber.Bounce`** is the back-channel. Once the consecutive
  non-empty poll count crosses the threshold (default 5), the FanOut
  calls the bounce callback you supplied — typically `subscriber.Bounce`
  — and resets the streak so the next round of measurements starts from
  zero against the freshly reconnected listener.

A single `Subscriber` can be shared by many FanOuts; each FanOut decides
independently when to bounce. Multiple bounces during a single outage
coalesce — `Subscriber.Bounce` is a non-blocking send on a buffered
channel, and the listen loop drains any pending signal at the start of
each fresh connection.

### Metrics

`EnableRecovery` registers two metrics through an
`elephantine.MetricsHelper`:

- `pg_fanout_<channel>_poll_saved_total` (counter) — every item that
  arrived via the fallback poll. Useful for long-term graphing of
  notification dropouts; counted per item so a single fat drain shows
  its true volume.
- `pg_fanout_<channel>_poll_saved_streak` (gauge) — the number of
  *consecutive non-empty fallback-poll calls* since the last wire-side
  notification. One backlog-clearing poll counts as one, regardless of
  how many items it drained. Resets to 0 on each successful delivery
  from the LISTEN connection or after a bounce.

The channel name is sanitized to a Prometheus-safe segment (characters
outside `[a-zA-Z0-9_]` become `_`).

The streak gauge is the right thing to alert on. Example PromQL:

```
pg_fanout_outbox_new_poll_saved_streak > 0
```

Hold for a few minutes to avoid flapping. The counter is for capacity
planning ("how often is this happening overall?") and post-incident
analysis.

## Wiring it up

A consumer that wants the recovery pattern needs:

1. A FanOut for the notification channel.
2. A `Subscriber` (typically shared across all FanOuts in the process).
3. A `MetricsHelper` so the FanOut can register its metrics.
4. A poll loop with a fallback period (e.g. 5 seconds).

```go
const ChannelName = "outbox_new"

type Notification struct {
    BlogID uuid.UUID `json:"blog_id"`
}

// FanOut, Subscriber, recovery wiring. The Subscriber takes the FanOut as
// a ChannelSubscription; then we hand the Subscriber's Bounce back to the
// FanOut so the recovery loop closes.
fanout := pg.NewFanOut[Notification](ChannelName)
sub := pg.NewSubscriber(logger, pool, []pg.ChannelSubscription{fanout})

err := fanout.EnableRecovery(
    elephantine.NewMetricsHelper(registerer),
    sub.Bounce,
    // pg.WithBounceThreshold(5) is the default.
)
if err != nil {
    return fmt.Errorf("enable fanout recovery: %w", err)
}

// Consumer side: a small wakeup channel registered with the FanOut, plus
// a fallback poll ticker. The consumer reports poll findings via
// fanout.Polled; the wire-side reset happens automatically inside the
// FanOut.
events := make(chan Notification, 64)

go fanout.ListenAll(ctx, events)
go sub.Run(ctx)

ticker := time.NewTicker(5 * time.Second)
defer ticker.Stop()

_ = drain() // initial backlog catch-up on startup

for {
    select {
    case <-ctx.Done():
        return
    case <-ticker.C:
        n, err := drain()
        if err != nil {
            logger.Error("poll drain", "err", err)
            continue
        }
        fanout.Polled(n)
    case <-events:
        _, _ = drain()
        // No Notified call needed — the FanOut already reset the streak
        // when NotifyWithPayload dispatched this notification to us.
    }
}
```

### Channel buffer sizing

The FanOut listener's channel buffer (`events` above) needs to hold
exactly one pending wake-up. A "wake up and drain" consumer pulls all
available work on every wake-up, so it does not matter whether one or
one thousand wire notifications fired while the drain was in flight —
the next wake-up catches up the entire backlog. **Use a buffer of 1.**

The recovery streak is unaffected by listener drops. `NotifyWithPayload`
resets the streak as soon as it dispatches the message via `Notify`;
with the default `OverflowDrop` policy the dispatch returns nil even
when no listener had room to queue the message, and the reset still
fires. Drops at the listener therefore measure consumer throughput, not
wire health.

`FanOut` with the default `OverflowDrop` policy silently drops a
notification when the listener channel is full. That is the right
choice for a "wake up and drain" consumer — the payload is just a kick,
the actual work is whatever the drain finds.

### Publisher side

The publisher calls `fanout.Publish(ctx, db, payload)` after committing
the underlying write. `pg_notify` only fires at commit anyway; publishing
*after* the commit return keeps the publisher cheap and the consumer
behaviour predictable. A publish failure is tolerable because the
consumer's fallback poll will catch the orphaned write — log the error
and move on.

### `OnReconnect`

`Subscriber.WithOnReconnect` is called before each LISTEN is re-issued,
including after a bounce. Use it to nudge consumers that a reconnect
happened so they can poll-drain immediately rather than waiting for the
next tick. Keep the callback short — it runs synchronously inside the
listen loop and delays the next `LISTEN`.

## Tuning

| Knob | Default | When to change |
| ---- | ------- | -------------- |
| `WithBounceThreshold` | 5 | Lower for very latency-sensitive consumers (fires faster but more aggressively on transient blips). Higher for systems where publishers sometimes fall behind their writes, so racing polls can rack up the streak in normal operation. |
| Fallback poll period | consumer-defined (e.g. 5 s) | Shorter polls catch up faster after a missed notification but raise the rate at which a sick LISTEN can grow the streak — combined with `WithBounceThreshold`, this controls the time-to-bounce (≈ `threshold × poll_period` in the worst case). |
| `WithPingInterval` / `WithPingGrace` | 5 min / 7 min | Lower for connections that fail silently on shorter timescales (aggressive NAT/load balancer idle timeouts). |

## What this isn't

- It does not replace the ping-based health check. The ping detects fully
  dead connections; the recovery wiring detects "alive but not
  delivering."
- It does not retry individual notifications. A dropped notification is
  always recovered by the fallback poll, with at most one poll-period
  worth of latency.
- It does not coordinate across replicas. Each replica's FanOut bounces
  its own `Subscriber`. That is correct: a connection is bad or good per
  replica, not globally.
