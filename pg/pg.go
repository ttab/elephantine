package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ttab/elephantine"
)

type DBExec interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// PBool converts a *bool to a pgtype.Bool.
func PBool(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}

	return pgtype.Bool{
		Bool:  *b,
		Valid: true,
	}
}

// PInt32 converts a *int32 to a pgtype.Int4.
func PInt32(n *int32) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}

	return pgtype.Int4{
		Int32: *n,
		Valid: true,
	}
}

// Int64 converts a int64 to a pgtype.Int8.
func Int64(n int64) pgtype.Int8 {
	return pgtype.Int8{
		Int64: n,
		Valid: true,
	}
}

// PInt64 converts a *int64 to a pgtype.Int8.
func PInt64(n *int64) pgtype.Int8 {
	if n == nil {
		return pgtype.Int8{}
	}

	return pgtype.Int8{
		Int64: *n,
		Valid: true,
	}
}

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

// PUUID converts a *uuid.UUID to a pgtype.UUID.
func PUUID(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{
		Bytes: *u,
		Valid: true,
	}
}

// UUID converts a uuid.UUID to a pgtype.UUID.
func UUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: u,
		Valid: true,
	}
}

// ToUUIDPointer converts a pgtype.UUID to a *uuid.UUID.
func ToUUIDPointer(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}

	u := uuid.UUID(v.Bytes)

	return &u
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

// Text converts a string to a pgtype.Text.
func Text(s string) pgtype.Text {
	return pgtype.Text{
		String: s,
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
//
// Deprecated: use Rollback() instead.
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

// Rollback rolls back a transaction and joins the rollback error to the
// outError if the rollback fails. If the transaction already has been
// committed/closed it's not treated as an error.
//
// Defer a call to Rollback directly after a transaction has been created. That
// will give you the guarantee that everything you've done will be rolled back
// if you return early before committing.
func Rollback(tx pgx.Tx, outErr *error) {
	err := tx.Rollback(context.Background())
	if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		*outErr = errors.Join(*outErr, fmt.Errorf("roll back transaction: %w", err))
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

	maps.Copy(q, vars)

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
	ctx context.Context, pool TransactionBeginner,
	fn func(tx pgx.Tx) error,
) (outErr error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer Rollback(tx, &outErr)

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
