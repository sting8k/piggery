//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// install.ps1 against a fake release, run by Windows PowerShell as a user would run it: a binary
// whose sha256 is not the one in checksums.txt installs nothing; the right one is installed where
// PIGGERY_INSTALL_DIR says (a path with a space) and runs; and installing again over a piggery that
// is running puts the new binary in its place and the running one aside.
func TestInstallScript(t *testing.T) {
	bin, home := buildWithHome(t)
	asset, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	name := "piggery-windows-" + runtime.GOARCH + ".exe"
	sum := sha256.Sum256(asset)
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/download/" + name:
			w.Write(asset)
		case "/latest/download/checksums.txt":
			io.WriteString(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer release.Close()

	script, err := os.ReadFile(filepath.Join("..", "..", "install.ps1"))
	const releases = "https://github.com/sting8k/piggery/releases"
	if err != nil || !bytes.Contains(script, []byte(releases)) {
		t.Fatalf("install.ps1 does not download from %s: %v", releases, err)
	}
	local := filepath.Join(t.TempDir(), "install.ps1") // the same script, downloading from the server above
	if err := os.WriteFile(local, bytes.ReplaceAll(script, []byte(releases), []byte(release.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "my bin")
	target := filepath.Join(dir, "piggery.exe")
	install := func() (string, error) {
		cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
			"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", local)
		cmd.Env = append(append(os.Environ(), homeEnv(home)...), "PIGGERY_INSTALL_DIR="+dir, "PIGGERY_VERSION=")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	good := sums
	sums = strings.Repeat("0", 64) + "  " + name + "\n"
	if out, err := install(); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("a binary with the wrong checksum: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a binary with the wrong checksum was installed")
	}

	sums = good
	if out, err := install(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, asset) {
		t.Fatal("the installed binary is not the release's")
	}
	version := exec.Command(target, "--version")
	if out, err := version.CombinedOutput(); err != nil {
		t.Fatalf("the installed binary does not run: %v\n%s", err, out)
	}

	daemon := exec.Command(target, "serve") // a running piggery: its image cannot be replaced or deleted
	daemon.Env = append(os.Environ(), homeEnv(home)...)
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		daemon.Process.Kill()
		daemon.Wait() // its image is unlocked before the temp dir is removed
	}()
	if out, err := install(); err != nil {
		t.Fatalf("install over a running piggery: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, asset) {
		t.Fatal("after installing over a running piggery the binary is not the release's")
	}
	if _, err := os.Stat(target + ".old"); err != nil {
		t.Fatalf("the running binary was not moved aside: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Fatalf("%d files in the install dir; want the binary and the one moved aside", len(ents))
	}
}
