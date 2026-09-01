package pg_test

import (
	"testing"

	"github.com/ttab/mage/libschema"
)

// TestSchemaMatchesTheMigrations keeps the two copies of this package's DDL in
// step. The migrations in pg/schema are the source: they are what a consuming
// service vendors into its own ./schema, because neither `mage sql:migrate`
// nor elephant-platform's `setup db migrate` looks inside a dependency for a
// migration. pg/schema.sql is what sqlc reads, and it is generated from them.
//
// Without this test the library holds the same DDL twice with nothing keeping
// the copies together, which is exactly the drift that made hand-copying
// job_lock into six services error-prone in the first place — one level in.
func TestSchemaMatchesTheMigrations(t *testing.T) {
	err := libschema.CheckFlattened("schema", "schema.sql")
	if err != nil {
		t.Fatalf("run `mage sql:librarySchema pg/schema pg/schema.sql`: %v", err)
	}
}
