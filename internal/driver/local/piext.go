package local

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	piext "github.com/sting8k/piggery/extensions/pi"
)

// piggery's pi extension installed from the binary:
// the embedded files unpacked into <pi agent dir>/extensions/piggery, which pi loads by itself.
// The first line of its index.ts marks it as piggery's and names the version that wrote it; a
// directory there without that line is not piggery's to replace or remove.

const piExtMarker = "// managed by piggery "

// PiExtDir is where `piggery setup pi` installs the extension.
func PiExtDir(dir string) string {
	return filepath.Join(HumanAgentDir(AgentDirRoot(dir)), "extensions", "piggery")
}

// piExtFiles are the files of an installed copy written by version.
func piExtFiles(version string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(piext.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := piext.Files.ReadFile(p)
		if p == "index.ts" {
			b = append([]byte(piExtMarker+version+": written by `piggery setup pi`; run it again or `piggery setup remove pi` instead of editing\n"), b...)
		}
		files[p] = b
		return err
	})
	return files, err
}

// PiExtVersion is the version of the managed copy at ext; managed is false when ext holds none.
func PiExtVersion(ext string) (version string, managed bool) {
	b, err := os.ReadFile(filepath.Join(ext, "index.ts"))
	if err != nil || !bytes.HasPrefix(b, []byte(piExtMarker)) {
		return "", false
	}
	line, _, _ := strings.Cut(string(b[len(piExtMarker):]), "\n")
	v, _, _ := strings.Cut(line, ":")
	return v, true
}

// PiExtCurrent: the managed copy at ext holds exactly what version would write.
func PiExtCurrent(ext, version string) bool {
	files, err := piExtFiles(version)
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(ext)
	if err != nil || len(entries) != len(files) {
		return false
	}
	for name, want := range files {
		if got, err := os.ReadFile(filepath.Join(ext, name)); err != nil || !bytes.Equal(got, want) {
			return false
		}
	}
	return true
}

// InstallPiExt writes the extension into ext, in place of a managed copy or of a symlink (an
// older way to add piggery). Anything else there is refused. It reports whether it wrote.
func InstallPiExt(ext, version string) (bool, error) {
	if fi, err := os.Lstat(ext); err == nil {
		if _, managed := PiExtVersion(ext); fi.Mode()&os.ModeSymlink == 0 && !managed {
			return false, fmt.Errorf("%s is not piggery's (no %q line): move it away, or use --ext", ext, strings.TrimSpace(piExtMarker))
		}
		if PiExtCurrent(ext, version) {
			return false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	files, err := piExtFiles(version)
	if err != nil {
		return false, err
	}
	tmp := ext + ".piggery-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return false, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return false, err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o644); err != nil {
			return false, err
		}
	}
	if err := os.RemoveAll(ext); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, ext)
}

// UpdatePiExt (daemon start, like the templates): rewrites a managed copy at ext that is older
// than version, or, when either is a dev build, that differs from what version writes. No copy,
// no change.
func UpdatePiExt(ext, version string) (bool, error) {
	have, managed := PiExtVersion(ext)
	if !managed || PiExtCurrent(ext, version) {
		return false, nil
	}
	if a, okA := parseSemver(have); okA {
		if b, okB := parseSemver(version); okB && !semverLess(a, b) {
			return false, nil // the copy is as new or newer: another binary wrote it
		}
	}
	return InstallPiExt(ext, version)
}

// RemovePiExt deletes a managed copy (or a symlink) at ext; it reports whether there was one.
func RemovePiExt(ext string) (bool, error) {
	fi, err := os.Lstat(ext)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, managed := PiExtVersion(ext); fi.Mode()&os.ModeSymlink == 0 && !managed {
		return false, fmt.Errorf("%s is not piggery's: left as it is", ext)
	}
	return true, os.RemoveAll(ext)
}

// parseSemver reads vX.Y.Z (a release); dev builds are not.
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
