package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "28" {
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

// v20 -> v21 drops mail threads and expects_reply, keeps reply_to, and releases the messages held by
// the two removed limits (max_hops, messages_per_thread) with a routing cc copy of one; a message
// held by the rate limit stays held.
func TestMigrateV21ReleasesRemovedHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "piggery.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i, ddl := range migrations[:20] {
		if _, err := old.Exec(ddl); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	if _, err := old.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', '20')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value;
		INSERT INTO teams(id, name, model_name, manifest, root_cwd, created_at) VALUES ('T1', 't', 't', 'model: t', '/x', 1);
		INSERT INTO messages(id, seq, team_id, from_id, to_id, thread_id, reply_to, expects_reply, body, created_at, held_reason, cc_of) VALUES
		  ('M1', 1, 'T1', 'a', 'b', 'M1', NULL, 1, 'first', 1, NULL, NULL),
		  ('M2', 2, 'T1', 'b', 'a', 'M1', 'M1', 0, 'held by max_hops', 2, 'policy_hold', NULL),
		  ('M3', 3, 'T1', 'b', 'c', 'M1', NULL, 0, 'cc copy of M2', 2, 'policy_hold', 'M2'),
		  ('M4', 4, 'T1', 'a', 'b', 'M4', NULL, 0, 'held by messages_per_thread', 3, 'policy_hold', NULL),
		  ('M5', 5, 'T1', 'a', 'b', 'M5', NULL, 0, 'held by the rate limit', 4, 'policy_hold', NULL);
		INSERT INTO events(ts, type, ref_id, payload) VALUES
		  (2, 'held', 'M2', '{"rule_id":"limits.max_hops","reason":"policy_hold"}'),
		  (3, 'held', 'M4', '{"rule_id":"limits.messages_per_thread","reason":"policy_hold"}'),
		  (4, 'held', 'M5', '{"rule_id":"limits.messages_per_participant_per_minute","reason":"policy_hold"}')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var held, replyTo string
	if err := db.QueryRow(`SELECT COALESCE(GROUP_CONCAT(id), '') FROM messages WHERE held_reason IS NOT NULL`).Scan(&held); err != nil || held != "M5" {
		t.Fatalf("still held = %q, %v; want only M5 (the rate limit's)", held, err)
	}
	if err := db.QueryRow(`SELECT reply_to FROM messages WHERE id='M2'`).Scan(&replyTo); err != nil || replyTo != "M1" {
		t.Fatalf("reply_to of M2 = %q, %v; want M1 kept", replyTo, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name IN ('thread_id', 'expects_reply')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("thread_id/expects_reply columns left: %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='messages_thread'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages_thread index left: %d, %v", n, err)
	}
}

// Before migrating an existing DB the daemon writes a consistent copy named by its from/to versions,
// keeps the 3 newest of them and never touches other files in backups/; a fresh DB makes none, and
// a backup that cannot be written stops the migration with the path it tried.
func TestOpenBacksUpBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "piggery.db")
	backups := filepath.Join(dir, "backups")
	at := func(version int) { // a WAL DB left at schema `version`, with a marker row
		t.Helper()
		for _, f := range []string{path, path + "-wal", path + "-shm"} {
			os.Remove(f)
		}
		old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		defer old.Close()
		for i, ddl := range migrations[:version] {
			if _, err := old.Exec(ddl); err != nil {
				t.Fatalf("v%d: %v", i+1, err)
			}
		}
		if _, err := old.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', ?), ('marker', 'kept')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, version); err != nil {
			t.Fatal(err)
		}
	}
	names := func() string {
		t.Helper()
		es, err := os.ReadDir(backups)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, strings.SplitN(e.Name(), "-20", 2)[0])
		}
		return strings.Join(out, " ")
	}

	fresh, err := Open(filepath.Join(t.TempDir(), "piggery.db"))
	if err != nil {
		t.Fatal(err)
	}
	fresh.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "backups")); err == nil {
		t.Fatal("backups exists before any upgrade")
	}

	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mine.db", "pre-v3-4-by-hand.db"} { // not the daemon's pattern
		if err := os.WriteFile(filepath.Join(backups, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []int{17, 18, 19, 20} {
		at(v)
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	if got, want := names(), "mine.db pre-v18-28 pre-v19-28 pre-v20-28 pre-v3-4-by-hand.db"; got != want {
		t.Fatalf("backups = %q; want the 3 newest upgrades, and both other files kept", got)
	}
	es, _ := filepath.Glob(filepath.Join(backups, "pre-v20-28-*.db"))
	cp, err := sql.Open("sqlite", "file:"+es[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var version, marker string
	if err := cp.QueryRow(`SELECT (SELECT value FROM meta WHERE key='schema_version'), (SELECT value FROM meta WHERE key='marker')`).
		Scan(&version, &marker); err != nil || version != "20" || marker != "kept" {
		t.Fatalf("backup is at v%s with marker %q, %v; want v20 with its data", version, marker, err)
	}

	at(20)
	os.RemoveAll(backups)
	if err := os.WriteFile(backups, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), backups) {
		t.Fatalf("Open with an unwritable backups dir = %v; want an error naming %s", err, backups)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "20" {
		t.Fatalf("after the failed backup the DB is at v%s, %v; want it left at v20", version, err)
	}
}

// A v21 DB (the team's template name in model_name, its manifest snapshot with `model:`) opens at the
// latest version with the column renamed and the snapshot untouched (the parser reads the old key).
func TestOpenRenamesTheTeamTemplateColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "piggery.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for i, ddl := range migrations[:21] {
		if _, err := old.Exec(ddl); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	if _, err := old.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', '21')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value;
		INSERT INTO teams(id, name, model_name, manifest, root_cwd, created_at) VALUES ('T1', 't', 'plan', 'model: plan', '/x', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tpl, manifest string
	if err := db.QueryRow(`SELECT template_name, manifest FROM teams WHERE id='T1'`).Scan(&tpl, &manifest); err != nil || tpl != "plan" || manifest != "model: plan" {
		t.Fatalf("team after the migration: %q %q, %v", tpl, manifest, err)
	}
}
