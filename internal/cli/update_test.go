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
	skipOnWindows(t, "replaces the binary through a symlink and checks its mode bits (TestUpdateOnWindowsMovesTheOldBinaryAside is the Windows path)")
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

// On Windows the asset is piggery-windows-<arch>.exe, and the binary it replaces (which may be
// running: Windows lets a running program be renamed, not replaced) steps aside as piggery.exe.old,
// where the next update replaces it. goos is the updater's, so every OS runs this.
func TestUpdateOnWindowsMovesTheOldBinaryAside(t *testing.T) {
	bins := map[string][]byte{"v0.2.0": []byte("piggery v0.2.0 windows amd64"), "v0.3.0": []byte("piggery v0.3.0 windows amd64")}
	tag := "v0.2.0"
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
			{"name": "piggery-windows-amd64", "browser_download_url": srv.URL + "/noext"},
			{"name": "piggery-windows-amd64.exe", "browser_download_url": srv.URL + "/exe"},
			{"name": "checksums.txt", "browser_download_url": srv.URL + "/sums"},
		}})
	})
	mux.HandleFunc("/exe", func(w http.ResponseWriter, _ *http.Request) { w.Write(bins[tag]) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) {
		s := sha256.Sum256(bins[tag])
		io.WriteString(w, hex.EncodeToString(s[:])+"  piggery-windows-amd64.exe\n")
	})

	dir := t.TempDir()
	exe := filepath.Join(dir, "piggery.exe")
	if err := os.WriteFile(exe, []byte("piggery v0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := Version
	defer func() { Version = old }()
	u := updater{api: srv.URL + "/latest", exe: exe, goos: "windows", goarch: "amd64", client: srv.Client()}
	for _, step := range []struct{ from, to, aside string }{
		{"v0.1.0", "v0.2.0", "piggery v0.1.0"},
		{"v0.2.0", "v0.3.0", "piggery v0.2.0 windows amd64"},
	} {
		Version, tag = step.from, step.to
		if replaced, err := u.run(context.Background(), io.Discard, false, false); err != nil || !replaced {
			t.Fatalf("update to %s = %v, %v", step.to, replaced, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != string(bins[step.to]) {
			t.Fatalf("after the update to %s the binary is %q", step.to, b)
		}
		if b, _ := os.ReadFile(exe + ".old"); string(b) != step.aside {
			t.Fatalf("after the update to %s piggery.exe.old is %q; want the binary it replaced", step.to, b)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 2 {
			t.Fatalf("after the update to %s: %d files; want the binary and the one moved aside", step.to, len(ents))
		}
	}
}
