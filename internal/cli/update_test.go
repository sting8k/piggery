package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update against a fake GitHub API: --check writes nothing; a binary whose sha256 is not the
// one in checksums.txt is refused and the running binary stays; the right one replaces it (the
// asset for this os/arch, through a symlink to the binary, keeping its mode).
func TestUpdate(t *testing.T) {
	skipOnWindows(t, "piggery update for Windows is a later step: no Windows asset, and a running .exe cannot be replaced in place")
	newBin, otherBin := []byte("piggery v0.2.0 linux arm64"), []byte("piggery v0.2.0 darwin arm64")
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	sums := sum(otherBin) + "  piggery-darwin-arm64\n" + sum(newBin) + "  piggery-linux-arm64\n"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0", "assets": []map[string]string{
			{"name": "piggery-darwin-arm64", "browser_download_url": srv.URL + "/darwin"},
			{"name": "piggery-linux-arm64", "browser_download_url": srv.URL + "/linux"},
			{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"},
		}})
	})
	mux.HandleFunc("/darwin", func(w http.ResponseWriter, _ *http.Request) { w.Write(otherBin) })
	mux.HandleFunc("/linux", func(w http.ResponseWriter, _ *http.Request) { w.Write(newBin) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, sums) })

	dir := t.TempDir()
	exe := filepath.Join(dir, "piggery")
	if err := os.WriteFile(exe, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "piggery")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	old := Version
	Version = "v0.1.0"
	defer func() { Version = old }()
	u := updater{api: srv.URL + "/latest", exe: link, goos: "linux", goarch: "arm64", client: srv.Client()}
	unchanged := func(when string) {
		t.Helper()
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Fatalf("%s: binary is %q; want it unchanged", when, b)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 1 {
			t.Fatalf("%s: %d files beside the binary; want no temp file left", when, len(ents))
		}
	}

	var out strings.Builder
	if replaced, err := u.run(context.Background(), &out, true, false); err != nil || replaced ||
		out.String() != "current v0.1.0, latest v0.2.0\nv0.2.0 available: piggery update\n" {
		t.Fatalf("--check = %v, %v, %q", replaced, err, out.String())
	}
	unchanged("--check")

	sums = strings.Replace(sums, sum(newBin), sum([]byte("tampered")), 1)
	if replaced, err := u.run(context.Background(), io.Discard, false, false); err == nil || replaced ||
		!strings.Contains(err.Error(), "does not match checksums.txt") {
		t.Fatalf("bad checksum = %v, %v; want refused", replaced, err)
	}
	unchanged("bad checksum")

	sums = strings.Replace(sums, sum([]byte("tampered")), sum(newBin), 1)
	out.Reset()
	if replaced, err := u.run(context.Background(), &out, false, false); err != nil || !replaced || out.String() != "updated v0.1.0 → v0.2.0\n" {
		t.Fatalf("update = %v, %v, %q", replaced, err, out.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatalf("binary after update = %q; want the linux/arm64 asset", b)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced instead of the binary it points to")
	}
	if st, _ := os.Stat(exe); st.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v; want the old binary's 0750", st.Mode().Perm())
	}
}
