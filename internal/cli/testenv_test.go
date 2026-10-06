package cli

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// shortTmp is where a test makes the short dir of a daemon: unix socket paths are length-limited
// (104 bytes on macOS), so /tmp. Windows has no /tmp, and its pipe name is a hash.
func shortTmp() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/tmp"
}

// setHome makes dir the user's home: HOME on unix, USERPROFILE on Windows (os.UserHomeDir).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// linkExe puts this test binary in dir as the program name, to stand in for it on PATH: a symlink
// on unix, a copy named name.exe on Windows (a symlink needs a privilege there, and PATH lookup
// wants the .exe).
func linkExe(t *testing.T, dir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		os.Symlink(exe, filepath.Join(dir, name))
		return
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(dir, name+".exe"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
}

// permIs: a file's permission bits are what a test set; Windows keeps no such bits.
func permIs(got, want os.FileMode) bool { return runtime.GOOS == "windows" || got == want }

// skipOnWindows skips a test of behavior that is Unix-only, saying why.
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}
