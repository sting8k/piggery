package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	paseoext "github.com/sting8k/piggery/extensions/paseo"
)

// piggery's Paseo plugin (piggery top inside Paseo): `piggery setup paseo` writes the files
// embedded in the binary to ~/.piggery/paseo, with a VERSION file naming the piggery that wrote
// them, and installs that directory in a Paseo daemon (`paseo plugin install`, --paseo-home or
// Paseo's default home). A directory there without VERSION is not piggery's to replace or remove.
// Its server/installed.ts names this binary (self), which Paseo compiles into the plugin: the
// plugin runs that piggery unless the user's Piggery setting in Paseo names another, so it does
// not depend on the Paseo daemon's PATH.
// Paseo is not a harness: no worker profile, hooks or wake.

const (
	paseoPluginID  = "piggery"             // the id in extensions/paseo/paseo-plugin.json
	paseoInstalled = "server/installed.ts" // `export const piggeryPath = "";` in the repo
)

var paseoTarget = setupTarget{name: "paseo", cmd: "paseo", install: installPaseo, remove: removePaseo, status: paseoStatus}

func paseoDir(dir string) string { return filepath.Join(dir, "paseo") }

// paseoFiles are the files of a copy written by version for the binary self, by slash path.
func paseoFiles(version, self string) (map[string][]byte, error) {
	files := map[string][]byte{"VERSION": []byte(version + "\n")}
	err := fs.WalkDir(paseoext.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := paseoext.Files.ReadFile(p)
		files[p] = b
		return err
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files[paseoInstalled]; !ok {
		return nil, fmt.Errorf("the embedded Paseo plugin has no %s", paseoInstalled)
	}
	q, _ := json.Marshal(self)
	files[paseoInstalled] = []byte("export const piggeryPath = " + string(q) + ";\n")
	return files, nil
}

var errStale = errors.New("stale")

// paseoCurrent: dst holds exactly files.
func paseoCurrent(dst string, files map[string][]byte) bool {
	n := 0
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		want, ok := files[filepath.ToSlash(rel)]
		if got, err := os.ReadFile(p); !ok || err != nil || !bytes.Equal(got, want) {
			return errStale
		}
		n++
		return nil
	})
	return err == nil && n == len(files)
}

// writePaseoPlugin writes the copy of version for self into dst unless it is there already, in
// place of an earlier copy; it reports whether it wrote.
func writePaseoPlugin(dst, version, self string) (bool, error) {
	files, err := paseoFiles(version, self)
	if err != nil {
		return false, err
	}
	if paseoCurrent(dst, files) {
		return false, nil
	}
	if _, err := os.Stat(dst); err == nil {
		if _, err := os.Stat(filepath.Join(dst, "VERSION")); err != nil {
			return false, fmt.Errorf("%s is not piggery's (no VERSION): move it away", dst)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	tmp := dst + ".piggery-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return false, err
	}
	for name, b := range files {
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return false, err
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, dst)
}

// paseoRun runs paseo against home ("" = Paseo's default); a failure carries Paseo's message.
func paseoRun(home string, args ...string) ([]byte, error) {
	if home != "" {
		args = append(args, "--home", home)
	}
	out, err := exec.Command("paseo", args...).CombinedOutput()
	if err != nil {
		msg := string(bytes.TrimSpace(out))
		var e struct{ Error struct{ Message string } }
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return out, fmt.Errorf("paseo %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

type paseoPlugin struct {
	ID, Path, Status string
	Enabled          bool
}

// paseoListed is piggery's plugin as the Paseo daemon at home lists it (found false: none).
func paseoListed(home string) (p paseoPlugin, found bool, err error) {
	out, err := paseoRun(home, "plugin", "ls", "--json")
	if err != nil {
		return p, false, err
	}
	var ps []paseoPlugin
	if err := json.Unmarshal(out, &ps); err != nil {
		return p, false, fmt.Errorf("paseo plugin ls: %w", err)
	}
	for _, p := range ps {
		if p.ID == paseoPluginID {
			return p, true, nil
		}
	}
	return p, false, nil
}

// homeFlag is how a fix command names the Paseo home: flag is --paseo-home or --home.
func homeFlag(flag, home string) string {
	if home == "" {
		return ""
	}
	return " " + flag + " " + home
}

// installPaseo writes the plugin to ~/.piggery/paseo and has Paseo install it (in place of one from
// elsewhere: Paseo refuses an id it has), enable it, or reload it when the files changed; nothing
// when all is so already.
func installPaseo(o setupOpts) (string, error) {
	if _, err := exec.LookPath("paseo"); err != nil {
		return "", errors.New("paseo: `paseo` is not on PATH; install Paseo first")
	}
	dst := paseoDir(o.dir)
	wrote, err := writePaseoPlugin(dst, Version, o.self)
	if err != nil {
		return "", err
	}
	p, found, err := paseoListed(o.paseoHome)
	if err != nil {
		return "", err
	}
	var msgs []string
	if wrote {
		msgs = append(msgs, fmt.Sprintf("paseo: wrote piggery's plugin (%s, running %s) to %s", Version, o.self, dst))
	}
	run := func(args ...string) error {
		if _, err := paseoRun(o.paseoHome, args...); err != nil {
			return err
		}
		msgs = append(msgs, "paseo: ran paseo "+strings.Join(args, " ")+homeFlag("--home", o.paseoHome))
		return nil
	}
	var cmds [][]string
	switch {
	case found && !samePath(p.Path, dst):
		msgs = append(msgs, "paseo: in place of the plugin from "+p.Path)
		cmds = [][]string{{"plugin", "remove", paseoPluginID}, {"plugin", "install", dst}}
	case !found:
		cmds = [][]string{{"plugin", "install", dst}}
	case !p.Enabled:
		cmds = [][]string{{"plugin", "enable", paseoPluginID}}
	case wrote:
		cmds = [][]string{{"plugin", "reload", paseoPluginID}}
	}
	for _, c := range cmds {
		if err := run(c...); err != nil {
			return "", err
		}
	}
	if len(msgs) == 0 {
		return "paseo: piggery's plugin is already installed", nil
	}
	return strings.Join(msgs, "\n") + "\npaseo: reload the Paseo app to see it.", nil
}

// removePaseo has Paseo remove the plugin when it is setup's (from ~/.piggery/paseo), then removes
// that directory.
func removePaseo(o setupOpts) (string, error) {
	dst := paseoDir(o.dir)
	_, statErr := os.Stat(filepath.Join(dst, "VERSION"))
	var did []string
	if _, err := exec.LookPath("paseo"); err != nil {
		if statErr != nil {
			return "paseo: piggery's plugin is not installed", nil
		}
		return "", errors.New("paseo: `paseo` is not on PATH; cannot remove piggery's plugin from Paseo")
	}
	p, found, err := paseoListed(o.paseoHome)
	if err != nil {
		return "", err
	}
	switch {
	case found && samePath(p.Path, dst):
		if _, err := paseoRun(o.paseoHome, "plugin", "remove", paseoPluginID); err != nil {
			return "", err
		}
		did = append(did, "ran paseo plugin remove "+paseoPluginID+homeFlag("--home", o.paseoHome))
	case found:
		did = append(did, "left the plugin from "+p.Path+" (not setup's): `paseo plugin remove "+paseoPluginID+"`")
	}
	if statErr == nil {
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
		did = append(did, "removed "+dst)
	}
	if len(did) == 0 {
		return "paseo: piggery's plugin is not installed", nil
	}
	return "paseo: " + strings.Join(did, "\npaseo: "), nil
}

// paseoStatus: piggery's plugin in the Paseo daemon, enabled, from ~/.piggery/paseo as this
// piggery writes it. Without paseo on PATH it is only the hint of the status line.
func paseoStatus(o setupOpts) harnessState {
	st := harnessState{Name: "paseo"}
	if _, err := exec.LookPath("paseo"); err != nil {
		return st
	}
	dst := paseoDir(o.dir)
	p, found, err := paseoListed(o.paseoHome)
	if err != nil {
		if _, serr := os.Stat(dst); serr == nil {
			st.Problems = append(st.Problems, problem{err.Error(), "start the Paseo daemon"})
		}
		return st
	}
	if !found {
		return st
	}
	st.Installed = true
	parts := []string{p.Status}
	fix := "piggery setup paseo" + homeFlag("--paseo-home", o.paseoHome)
	if !samePath(p.Path, dst) {
		parts = append(parts, "from "+p.Path)
	} else if files, err := paseoFiles(Version, o.self); err == nil && !paseoCurrent(dst, files) {
		have, _ := os.ReadFile(filepath.Join(dst, "VERSION"))
		st.Problems = append(st.Problems, problem{fmt.Sprintf("the plugin in %s (piggery %s) is not what this piggery (%s, %s) writes",
			dst, firstNonEmpty(strings.TrimSpace(string(have)), "(unknown)"), Version, o.self), fix})
	}
	if !p.Enabled {
		st.Problems = append(st.Problems, problem{"the plugin is disabled", "paseo plugin enable " + paseoPluginID + homeFlag("--home", o.paseoHome)})
	}
	st.Detail = strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), ", ")
	return st
}
