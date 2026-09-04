-- Generated from this package's tern migrations by
-- `mage sql:librarySchema`. Do not edit.

CREATE TABLE job_lock (
    name text NOT NULL PRIMARY KEY,
    holder text NOT NULL,
    touched timestamp with time zone NOT NULL,
    iteration bigint NOT NULL
);
