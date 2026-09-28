// Package store opens the piggery SQLite database and applies the schema.
// Only the daemon opens the DB; clients never do.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed migrate_2.sql
var migrate2 string

//go:embed migrate_3.sql
var migrate3 string

//go:embed migrate_4.sql
var migrate4 string

//go:embed migrate_5.sql
var migrate5 string

//go:embed migrate_6.sql
var migrate6 string

//go:embed migrate_7.sql
var migrate7 string

//go:embed migrate_8.sql
var migrate8 string

//go:embed migrate_9.sql
var migrate9 string

//go:embed migrate_10.sql
var migrate10 string

//go:embed migrate_11.sql
var migrate11 string

//go:embed migrate_12.sql
var migrate12 string

//go:embed migrate_13.sql
var migrate13 string

//go:embed migrate_14.sql
var migrate14 string

//go:embed migrate_15.sql
var migrate15 string

//go:embed migrate_16.sql
var migrate16 string

//go:embed migrate_17.sql
var migrate17 string

//go:embed migrate_18.sql
var migrate18 string

//go:embed migrate_19.sql
var migrate19 string

// migrations[i] takes the DB from version i to i+1. schema.sql is v1; never edit an applied step.
var migrations = []string{schema, migrate2, migrate3, migrate4, migrate5, migrate6, migrate7, migrate8, migrate9, migrate10, migrate11, migrate12, migrate13, migrate14, migrate15, migrate16, migrate17, migrate18, migrate19}

// Open opens (creating if needed) the DB file at path with mode 0600, WAL, and the schema applied.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return open("file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
}

// OpenMemory opens a private in-memory DB with the schema applied (tests).
func OpenMemory() (*sql.DB, error) {
	return open("file::memory:?_pragma=foreign_keys(1)")
}

func open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single writer (the daemon); one connection also keeps :memory: a single DB.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate brings the DB to the latest version, one transaction per step.
func migrate(db *sql.DB) error {
	var hasMeta int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&hasMeta); err != nil {
		return err
	}
	version := 0
	if hasMeta == 1 {
		var v string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
			return fmt.Errorf("store: read schema version: %w", err)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > len(migrations) {
			return fmt.Errorf("store: unsupported schema version %q", v)
		}
		version = n
	}
	for ; version < len(migrations); version++ {
		if err := step(db, migrations[version], version+1); err != nil {
			return fmt.Errorf("store: migrate to v%d: %w", version+1, err)
		}
	}
	return nil
}

func step(db *sql.DB, ddl string, to int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(ddl); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(to)); err != nil {
		return err
	}
	return tx.Commit()
}
