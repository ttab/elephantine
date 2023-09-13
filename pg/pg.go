package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ttab/elephantine"
)

// Date converts a stdlib time.Time to a pgtype.Date.
func Date(t time.Time) pgtype.Date {
	return pgtype.Date{
		Time:  t,
		Valid: true,
	}
}

// Time converts a stdlib time.Time to a pgtype.Timestamptz.
func Time(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{
		Time:  t,
		Valid: true,
	}
}

// PTime converts a stdlib *time.Time to a pgtype.Timestamptz.
func PTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}

	return pgtype.Timestamptz{
		Time:  *t,
		Valid: true,
	}
}

// Time converts a stdlib time.Time to a pgtype.Timestamptz, but will return a
// Timestamptz that represents a null value in the database if t is zero.
func TimeOrNull(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}

	return pgtype.Timestamptz{
		Time:  t,
		Valid: true,
	}
}

// PText converts a *string to a pgtype.Text.
func PText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}

	return pgtype.Text{
		String: *s,
		Valid:  true,
	}
}

// TextOrNull returns a pgtype.Text for the given string, but will return a Text
// value that represents null in the database if the string is empty.
func TextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}

	return pgtype.Text{
		String: s,
		Valid:  true,
	}
}

// BigintOrNull returns a pgtype.Int8 for the given value, but will return a Int8
// value that represents null in the database if the value is zero.
func BigintOrNull(n int64) pgtype.Int8 {
	if n == 0 {
		return pgtype.Int8{}
	}

	return pgtype.Int8{
		Int64: n,
		Valid: true,
	}
}

// PInt2 returns a pgtype.Int2 for the given value, but will return a Int2 value
// that represents null in the database if the value is nil.
func PInt2(n *int16) pgtype.Int2 {
	if n == nil {
		return pgtype.Int2{}
	}

	return pgtype.Int2{
		Int16: *n,
		Valid: true,
	}
}

// SafeRollback rolls back a transaction and logs if the rollback fails. If the
// transaction already has been closed it's not treated as an error.
func SafeRollback(
	ctx context.Context, logger *slog.Logger, tx pgx.Tx, txName string,
) {
	err := tx.Rollback(context.Background())
	if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		logger.ErrorContext(ctx, "failed to roll back",
			elephantine.LogKeyError, err,
			elephantine.LogKeyTransaction, txName)
	}
}

// SetConnStringVariables parses a connection string URI and adds the given
// query string variables to it.
func SetConnStringVariables(conn string, vars url.Values) (string, error) {
	u, err := url.Parse(conn)
	if err != nil {
		return "", fmt.Errorf("not a valid URI: %w", err)
	}

	if u.Scheme != "postgres" {
		return "", fmt.Errorf("%q is not a postgres:// URI", conn)
	}

	q := u.Query()

	for k, v := range vars {
		q[k] = v
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

// IsConstraintError checks if an error was caused by a specific constraint
// violation.
func IsConstraintError(err error, constraint string) bool {
	if err == nil {
		return false
	}

	var pgerr *pgconn.PgError

	ok := errors.As(err, &pgerr)
	if !ok {
		return false
	}

	return pgerr.ConstraintName == constraint
}

// TransactionBeginner is the interface for something that can start a pgx
// transaction for use with WithTX().
type TransactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// WithTX starts a transaction and calls the given function with it. If the
// function returns an error or panics the transaction will be rolled back.
func WithTX(
	ctx context.Context, logger *slog.Logger, pool TransactionBeginner,
	name string, fn func(tx pgx.Tx) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer SafeRollback(ctx, logger, tx, name)

	err = fn(tx)
	if err != nil {
		return err
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}

	return nil
}
