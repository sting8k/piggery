package local

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	opencodeext "github.com/sting8k/piggery/extensions/opencode"
)

// piggery's opencode plugin installed from the binary (managedext.go): the tree of opencodeext.Tree
// under <piggery dir>/plugins/opencode, one copy for the sessions the Human opens (`piggery setup
// opencode` adds its entry to `plugin` in opencode's config) and for workers (OPENCODE_CONFIG_CONTENT
// names the same entry).

var opencodeManaged = managedExt{
	setup:      "opencode",
	markerFile: opencodeext.Entry,
	hint:       "move it away",
	files:      opencodeext.Tree,
}

// OpencodeExtDir is where the plugin is installed (`piggery setup opencode`, and a worker's start).
func OpencodeExtDir(dir string) string { return PluginDir(dir, "opencode") }

// OpencodeEntry is the file opencode loads as the plugin.
func OpencodeEntry(dir string) string {
	return filepath.Join(OpencodeExtDir(dir), filepath.FromSlash(opencodeext.Entry))
}

// OpencodePluginSpec is the `plugin` entry that loads the plugin at entry.
// A Windows path is C:\x\y, which a file URL spells file:///C:/x/y.
func OpencodePluginSpec(entry string) string {
	p := filepath.ToSlash(entry)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// OpencodeEntryOf is the path a `plugin` entry names when it is piggery's copy of the plugin (a file
// URL ending in plugins/opencode/<Entry>, wherever the piggery directory is); ok false for any other.
func OpencodeEntryOf(spec string) (path string, ok bool) {
	u, err := url.Parse(spec)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' { // /C:/x is C:\x
		p = p[1:]
	}
	return filepath.FromSlash(p), strings.HasSuffix(u.Path, "/plugins/opencode/"+opencodeext.Entry)
}

// OpencodeExtVersion is the integration version of the managed copy at ext; managed is false when
// ext holds none.
func OpencodeExtVersion(ext string) (version int, managed bool) { return opencodeManaged.Version(ext) }

// OpencodeExtCurrent: the managed copy at ext holds exactly what this binary would write.
func OpencodeExtCurrent(ext string) bool { return opencodeManaged.Current(ext) }

// InstallOpencodeExt writes the plugin into ext, in place of a managed copy. Anything else there is
// refused. It reports whether it wrote.
func InstallOpencodeExt(ext string) (bool, error) { return opencodeManaged.Install(ext) }

// UpdateOpencodeExt (daemon start): rewrites a managed copy at ext whose integration version is
// lower than this binary's. No copy, no change.
func UpdateOpencodeExt(ext string) (bool, error) { return opencodeManaged.Update(ext) }

// RemoveOpencodeExt deletes a managed copy at ext; it reports whether there was one.
func RemoveOpencodeExt(ext string) (bool, error) { return opencodeManaged.Remove(ext) }

var opencodeWorkerExt sync.Mutex

// EnsureOpencodeWorker makes the plugin copy current and returns its entry. Called before every
// spawn: the copy is written only when missing or not what this binary writes, never while current,
// so a worker that is starting is not raced by a rewrite.
func EnsureOpencodeWorker(dir string) (entry string, err error) {
	opencodeWorkerExt.Lock()
	defer opencodeWorkerExt.Unlock()
	if _, err := InstallOpencodeExt(OpencodeExtDir(dir)); err != nil {
		return "", err
	}
	return OpencodeEntry(dir), nil
}
