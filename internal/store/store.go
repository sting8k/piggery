// Package store opens the piggery SQLite database and applies the schema.
// Only the daemon opens the DB; clients never do.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

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

//go:embed migrate_20.sql
var migrate20 string

//go:embed migrate_21.sql
var migrate21 string

//go:embed migrate_22.sql
var migrate22 string

//go:embed migrate_23.sql
var migrate23 string

//go:embed migrate_24.sql
var migrate24 string

//go:embed migrate_25.sql
var migrate25 string

//go:embed migrate_26.sql
var migrate26 string

//go:embed migrate_27.sql
var migrate27 string

// migrations[i] takes the DB from version i to i+1. schema.sql is v1; never edit an applied step.
var migrations = []string{schema, migrate2, migrate3, migrate4, migrate5, migrate6, migrate7, migrate8, migrate9, migrate10, migrate11, migrate12, migrate13, migrate14, migrate15, migrate16, migrate17, migrate18, migrate19, migrate20, migrate21, migrate22, migrate23, migrate24, migrate25, migrate26, migrate27}

// Open opens (creating if needed) the DB file at path with mode 0600, WAL, and the schema applied.
// An existing DB below the latest schema version is first copied to backups/ next to it (backup).
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return open("file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		filepath.Join(filepath.Dir(path), "backups"))
}

// OpenMemory opens a private in-memory DB with the schema applied (tests).
func OpenMemory() (*sql.DB, error) {
	return open("file::memory:?_pragma=foreign_keys(1)", "")
}

// open applies the schema; backupDir "" makes no backup before a migration.
func open(dsn, backupDir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single writer (the daemon); one connection also keeps :memory: a single DB.
	db.SetMaxOpenConns(1)
	if err := migrate(db, backupDir); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate brings the DB to the latest version, one transaction per step. An existing DB (not a
// fresh one) that has steps to take is backed up into backupDir first.
func migrate(db *sql.DB, backupDir string) error {
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
		if version < len(migrations) && backupDir != "" {
			if err := backup(db, backupDir, version, len(migrations)); err != nil {
				return err
			}
		}
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

// keepBackups is how many pre-migration backups stay.
const keepBackups = 3

// backupName matches the backups backup writes: pre-v<from>-<to>-<YYYY-MM-DD>.db. Other files in
// the directory (hand-made backups) never match, so they are never pruned.
var backupName = regexp.MustCompile(`^pre-v(\d+)-(\d+)-(\d{4}-\d{2}-\d{2})\.db$`)

// backup writes a consistent copy of the DB (VACUUM INTO: a file copy of a WAL database is not
// consistent) to dir/pre-v<from>-<to>-<date>.db, then deletes all but the newest keepBackups
// files of that name pattern. A failure stops the migration and names the path.
func backup(db *sql.DB, dir string, from, to int) error {
	dst := filepath.Join(dir, fmt.Sprintf("pre-v%d-%d-%s.db", from, to, time.Now().Format("2006-01-02")))
	fail := func(err error) error {
		return fmt.Errorf("store: back up before migrating v%d to v%d, writing %s: %w", from, to, dst, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	tmp := dst + ".tmp" // VACUUM INTO needs a file that does not exist; a same-day retry replaces the old backup
	os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return fail(err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fail(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fail(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // the backup is written; only the pruning is skipped
	}
	var names []string
	for _, e := range entries {
		if backupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	key := func(name string) (date string, from, to int) {
		m := backupName.FindStringSubmatch(name)
		from, _ = strconv.Atoi(m[1])
		to, _ = strconv.Atoi(m[2])
		return m[3], from, to
	}
	sort.Slice(names, func(i, j int) bool { // newest first: date, then version
		di, fi, ti := key(names[i])
		dj, fj, tj := key(names[j])
		if di != dj {
			return di > dj
		}
		if ti != tj {
			return ti > tj
		}
		return fi > fj
	})
	for _, name := range names[min(keepBackups, len(names)):] {
		os.Remove(filepath.Join(dir, name))
	}
	return nil
}
