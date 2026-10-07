package local

import (
	"os"
	"path/filepath"
)

// What piggery keeps in its directory (default ~/.piggery), apart from the database, config and
// logs, has one place by kind, named by harness:
//
//	plugins/<name>/                          copies of piggery's own adapters that piggery installs
//	run/<name>/<participant>/<run>/          scratch of one run, deleted when the run ends (and
//	                                         <participant>/ with it once empty)
//	sessions/<name>/<participant or session>/  session data piggery owns, kept across runs
//
// gc removes a participant's entries by its id in logs/, run/ and sessions/ only (OwnPaths).

// PluginDir is where piggery's copy of harness name's adapter lives.
func PluginDir(dir, name string) string { return filepath.Join(dir, "plugins", name) }

// RunRoot holds harness name's per-run scratch: <RunRoot>/<participant>/<run>.
func RunRoot(dir, name string) string { return filepath.Join(dir, "run", name) }

// removeRun deletes run dir runDir (<RunRoot>/<participant>/<run>) and its participant's dir once
// that holds nothing else, so a worker that has stopped leaves no empty dir behind.
func removeRun(runDir string) {
	os.RemoveAll(runDir)
	os.Remove(filepath.Dir(runDir)) // fails, and keeps it, while anything else is in it
}

// SessionsRoot holds the session data piggery owns for harness name: <SessionsRoot>/<id>.
func SessionsRoot(dir, name string) string { return filepath.Join(dir, "sessions", name) }

// OwnPaths are the entries a participant (or a session) known by key may have in dir, for gc:
// its run logs and its entry under every harness in run/ and sessions/. They may not exist. The
// entries are found by listing, so key is only ever one path element joined under those three.
func OwnPaths(dir, key string) []string {
	out := []string{filepath.Join(dir, "logs", key)}
	for _, top := range []string{"run", "sessions"} {
		ents, _ := os.ReadDir(filepath.Join(dir, top))
		for _, e := range ents {
			if e.IsDir() {
				out = append(out, filepath.Join(dir, top, e.Name(), key))
			}
		}
	}
	return out
}
