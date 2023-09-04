UID := $(shell id -u)
GID := $(shell id -g)

SQL_TOOLS := ghcr.io/ttab/elephant-sqltools:v0.1.0

SQLC := docker run --rm \
	-v "${PWD}:/usr/src" -u $(UID):$(GID) \
	$(SQL_TOOLS) sqlc

.PHONY: generate
generate: pg/queries.sql.go

pg/queries.sql.go: bin/sqlc pg/schema.sql pg/queries.sql
	$(SQLC) --experimental generate
