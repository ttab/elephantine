package joblock

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/pg/joblock/internal/postgres"
	"github.com/ttab/eltest"
)

func TestMain(m *testing.M) {
	code := m.Run()

	err := eltest.PurgeBackingServices()
	if err != nil {
		log.Printf("failed to purge backing services: %v", err)
	}

	os.Exit(code)
}

// testPool creates a database with the job_lock table migrated into it.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	env := eltest.NewPostgres(t, eltest.Postgres18_6).Database(
		t, "joblock", os.DirFS("schema"), true)

	pool, err := pgxpool.New(t.Context(), env.PostgresURI)
	eltest.Must(t, err, "create the connection pool")

	t.Cleanup(pool.Close)

	return pool
}

func testLock(t *testing.T, pool *pgxpool.Pool, name string) *Lock {
	t.Helper()

	jl, err := New(pool, slog.New(slog.DiscardHandler), name, Options{
		PingInterval:      200 * time.Millisecond,
		StaleAfter:        800 * time.Millisecond,
		CheckInterval:     100 * time.Millisecond,
		Timeout:           100 * time.Millisecond,
		MetricsRegisterer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("create job lock: %v", err)
	}

	return jl
}

// TestPingSurvivesUnacknowledgedCommit verifies that a ping which committed
// without the holder learning of it doesn't cost the holder the lock. That is
// what a ping committing after its client-side timeout looks like from the
// row: the iteration has moved on and the holder is unchanged. It is
// simulated here by bumping the iteration directly while the lock is held.
func TestPingSurvivesUnacknowledgedCommit(t *testing.T) {
	pool := testPool(t)
	jl := testLock(t, pool, "unacknowledged")

	err := jl.RunWithContext(t.Context(), func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `
UPDATE job_lock SET iteration = iteration + 1 WHERE name = $1`,
			"unacknowledged")
		if err != nil {
			return fmt.Errorf("bump the iteration: %w", err)
		}

		// Several ping intervals, well short of the stale window
		// that a lost lock would otherwise sit out.
		select {
		case <-ctx.Done():
			return errors.New("lost the lock after an unacknowledged ping")
		case <-time.After(5 * jl.pingInterval):
		}

		var holder string

		err = pool.QueryRow(ctx, `
SELECT holder FROM job_lock WHERE name = $1`,
			"unacknowledged").Scan(&holder)
		if err != nil {
			return fmt.Errorf("read the holder: %w", err)
		}

		if holder != jl.Identity() {
			return errors.New("the row is no longer held by us")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAcquireRaceCommitsCleanly verifies that losing the race to insert the
// lock row is reported as not acquired, and leaves a transaction that
// commits. A unique violation aborts the transaction, which makes the commit
// of a race the code handles on purpose fail.
func TestAcquireRaceCommitsCleanly(t *testing.T) {
	ctx := t.Context()
	pool := testPool(t)

	winner := testLock(t, pool, "race")
	loser := testLock(t, pool, "race")

	winnerTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin winner transaction: %v", err)
	}

	t.Cleanup(func() {
		var err error

		pg.Rollback(winnerTx, &err)
		eltest.Must(t, err, "roll back the winner")
	})

	change, err := winner.acquire(ctx, postgres.New(winnerTx))
	if err != nil || !change.Ok {
		t.Fatalf("winner failed to acquire: ok=%v err=%v", change.Ok, err)
	}

	type result struct {
		change acquireChange
		err    error
	}

	done := make(chan result, 1)

	go func() {
		change, err := acquireAndCommit(ctx, pool, loser)

		done <- result{change: change, err: err}
	}()

	// The loser reads no row, since the winner's is uncommitted, and its
	// insert then waits on the winner's. Commit the winner only once the
	// insert is waiting, so that the test exercises the conflict rather
	// than a loser that found the row already there.
	waitForLockWait(t, pool)

	err = winnerTx.Commit(ctx)
	if err != nil {
		t.Fatalf("commit winner: %v", err)
	}

	res := <-done

	eltest.Must(t, res.err, "run the losing acquisition")

	if res.change.Ok {
		t.Fatal("loser acquired a lock the winner holds")
	}
}

// acquireAndCommit attempts to acquire the lock in a transaction of its own,
// and commits it.
func acquireAndCommit(
	ctx context.Context, pool *pgxpool.Pool, jl *Lock,
) (_ acquireChange, outErr error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return acquireChange{}, fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	change, err := jl.acquire(ctx, postgres.New(tx))
	if err != nil {
		return acquireChange{}, fmt.Errorf("acquire: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return acquireChange{}, fmt.Errorf("commit: %w", err)
	}

	return change, nil
}

// waitForLockWait waits until some other session in the database is blocked
// on a lock.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		var waiting bool

		err := pool.QueryRow(t.Context(), `
SELECT EXISTS (
       SELECT FROM pg_stat_activity
       WHERE datname = current_database()
             AND wait_event_type = 'Lock')`).Scan(&waiting)
		if err != nil {
			t.Fatalf("check for lock waits: %v", err)
		}

		if waiting {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for the loser to block on the winner's insert")
}
