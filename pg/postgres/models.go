// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.19.1

package postgres

import (
	"github.com/jackc/pgx/v5/pgtype"
)

type JobLock struct {
	Name      string
	Holder    string
	Touched   pgtype.Timestamptz
	Iteration int64
}
