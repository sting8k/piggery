package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A v1 DB file (as written by step 1) opens, migrates through every step (v2 processes, v3
// cc_of, v4/v5 add then drop the tool-streak columns) in place, and keeps its data.
func TestOpenMigratesV1File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "piggery.db")
	v1, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', '1');
		INSERT INTO teams(id, name, model_name, manifest, root_cwd, created_at) VALUES ('T1', 'p2p', 'p2p', 'model: p2p', '/x', 1)`); err != nil {
		t.Fatal(err)
	}
	v1.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, team string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "19" {
		t.Fatalf("version = %q, %v", version, err)
	}
	if err := db.QueryRow(`SELECT name FROM teams WHERE id='T1'`).Scan(&team); err != nil || team != "p2p" {
		t.Fatalf("v1 data lost: %q, %v", team, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='processes'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("processes table missing: %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='cc_of'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages.cc_of missing: %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('participants') WHERE name IN ('turns_no_tool','no_tool_streak')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("participants tool-streak columns: %d, %v", n, err)
	}
}
