package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/internal/jsonobj"
)

// pi: piggery's extension is either the copy the binary carries,
// unpacked into pi's extensions/piggery (the default; pi loads it by itself), or a checkout named
// in "extensions" of pi's settings.json (--ext, for development). Never both: installing one
// takes the other out. The pi worker profile's -e follows the one installed.

func piSettingsPath(dir string) string {
	return filepath.Join(local.HumanAgentDir(local.AgentDirRoot(dir)), "settings.json")
}

// piExtensions returns the settings object and its extension entries (nil when none).
func piExtensions(path string) (jsonobj.Object, []json.RawMessage, error) {
	b, err := readOptional(path)
	if err != nil {
		return nil, nil, err
	}
	o, err := jsonobj.Parse(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var exts []json.RawMessage
	if raw, ok := o.Get("extensions"); ok {
		if err := json.Unmarshal(raw, &exts); err != nil {
			return nil, nil, fmt.Errorf("%s: extensions: %w", path, err)
		}
	}
	return o, exts, nil
}

// piggeryEntry is the string of an extension entry when it is piggery's.
func piggeryEntry(raw json.RawMessage, path string) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	home, _ := os.UserHomeDir()
	return s, local.IsPiggeryExtension(s, home, filepath.Dir(path))
}

// installPi installs the binary's copy (ext "") or the checkout at ext, takes the other way out,
// and points the pi worker profile at it.
// piHarness: pi's extension talks to the daemon itself (no hooks, no piggery mcp).
var piHarness = harnessProfile{
	setupTarget: setupTarget{name: "pi", cmd: "pi",
		install: func(o setupOpts) (string, error) { return installPi(o.dir, o.ext, Version) },
		remove:  func(o setupOpts) (string, error) { return removePi(o.dir) },
		status:  func(o setupOpts) harnessState { return piStatus(o.dir, o.self, Version) },
	},
	profilePath: local.ProfilePath,
}

func installPi(dir, ext, version string) (string, error) {
	path := piSettingsPath(dir)
	var msgs []string
	index := filepath.Join(ext, "index.ts")
	if ext == "" {
		dst := local.PiExtDir(dir)
		index = filepath.Join(dst, "index.ts")
		wrote, err := local.InstallPiExt(dst, version)
		if err != nil {
			return "", err
		}
		old, _, err := setPiEntry(path, "")
		if err != nil {
			return "", err
		}
		if wrote {
			msgs = append(msgs, fmt.Sprintf("pi: installed piggery's extension (%s) in %s", version, dst))
		}
		if len(old) > 0 {
			msgs = append(msgs, fmt.Sprintf("pi: removed %s from %s (loaded twice otherwise)", strings.Join(old, ", "), path))
		}
	} else {
		old, changed, err := setPiEntry(path, ext)
		if err != nil {
			return "", err
		}
		if removed, err := local.RemovePiExt(local.PiExtDir(dir)); err != nil {
			return "", err
		} else if removed {
			msgs = append(msgs, "pi: removed the installed copy "+local.PiExtDir(dir))
		}
		if changed {
			msgs = append(msgs, fmt.Sprintf("pi: added %s to %s", ext, path))
		}
		if len(old) > 0 {
			msgs = append(msgs, "pi: in place of "+strings.Join(old, ", "))
		}
	}
	if changed, err := setPiProfileExt(dir, index); err != nil {
		return "", err
	} else if changed {
		msgs = append(msgs, "pi: workers load "+index+" ("+local.ProfilePath(dir)+")")
	}
	if len(msgs) == 0 {
		return "pi: piggery's extension is already installed", nil
	}
	return strings.Join(msgs, "\n") + "\npi: sessions started from now on join piggery; restart any that are open.", nil
}

// setPiEntry leaves ext as the only piggery entry of pi's settings ("" = none), and returns the
// piggery entries it took out and whether the file changed.
func setPiEntry(path, ext string) ([]string, bool, error) {
	o, exts, err := piExtensions(path)
	if err != nil {
		return nil, false, err
	}
	var out []json.RawMessage
	var old []string
	placed := ext == ""
	for _, raw := range exts {
		s, mine := piggeryEntry(raw, path)
		switch {
		case !mine:
			out = append(out, raw)
		case s == ext && !placed:
			out, placed = append(out, raw), true
		case !placed:
			out, placed = append(out, jsonobj.String(ext)), true
			old = append(old, s)
		default:
			old = append(old, s)
		}
	}
	if !placed {
		out = append(out, jsonobj.String(ext))
	}
	if len(old) == 0 && len(out) == len(exts) {
		return nil, false, nil
	}
	return old, true, writePiExtensions(path, o, out)
}

// removePi takes out both ways: the installed copy and piggery's settings entries.
func removePi(dir string) (string, error) {
	var msgs []string
	if removed, err := local.RemovePiExt(local.PiExtDir(dir)); err != nil {
		return "", err
	} else if removed {
		msgs = append(msgs, "pi: removed "+local.PiExtDir(dir))
	}
	path := piSettingsPath(dir)
	if old, _, err := setPiEntry(path, ""); err != nil {
		return "", err
	} else if len(old) > 0 {
		msgs = append(msgs, fmt.Sprintf("pi: removed %s from %s", strings.Join(old, ", "), path))
	}
	if len(msgs) == 0 {
		return "pi: piggery is not installed", nil
	}
	return strings.Join(msgs, "\n"), nil
}

// writePiExtensions writes o with exts ("extensions" goes when empty); the file keeps its final
// newline or its lack of one (pi writes none).
func writePiExtensions(path string, o jsonobj.Object, exts []json.RawMessage) error {
	if len(exts) == 0 {
		o = o.Del("extensions")
	} else {
		o = o.Set("extensions", rawArray(exts))
	}
	b := o.Bytes(0)
	if old, err := readOptional(path); err == nil && len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		b = bytes.TrimSuffix(b, []byte("\n"))
	}
	return writeFileAtomic(path, b)
}

func rawArray(items []json.RawMessage) json.RawMessage {
	parts := make([][]byte, len(items))
	for i, it := range items {
		parts[i] = it
	}
	return append(append([]byte("["), bytes.Join(parts, []byte(","))...), ']')
}

// setPiProfileExt points the pi worker profile's -e at index when it names piggery's extension
// (a profile the Human changed otherwise is left alone). No profile: nothing to do.
func setPiProfileExt(dir, index string) (bool, error) {
	path := local.ProfilePath(dir)
	b, err := readOptional(path)
	if err != nil || b == nil {
		return false, err
	}
	o, err := jsonobj.Parse(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	var args []string
	if raw, ok := o.Get("args"); !ok || json.Unmarshal(raw, &args) != nil {
		return false, nil
	}
	home, _ := os.UserHomeDir()
	changed := false
	for i := 1; i < len(args); i++ {
		if (args[i-1] == "-e" || args[i-1] == "--extension") && args[i] != index && local.IsPiggeryExtension(args[i], home, home) {
			args[i], changed = index, true
		}
	}
	if !changed {
		return false, nil
	}
	raw, _ := json.Marshal(args)
	return true, writeFileAtomic(path, o.Set("args", raw).Bytes(0))
}

// piStatus: the installed copy (current for this binary) or a settings entry, never both; the
// worker profile's extension present; `piggery` on PATH being this binary (the extension starts
// the daemon with it).
func piStatus(dir, self, version string) harnessState {
	st := harnessState{Name: "pi"}
	fix := "piggery setup pi"
	path := piSettingsPath(dir)
	_, exts, err := piExtensions(path)
	if err != nil {
		st.Problems = append(st.Problems, problem{err.Error(), ""})
		return st
	}
	var entries []string
	for _, raw := range exts {
		if s, ok := piggeryEntry(raw, path); ok {
			entries = append(entries, s)
		}
	}
	copyDir := local.PiExtDir(dir)
	have, managed := local.PiExtVersion(copyDir)
	switch {
	case managed && len(entries) > 0:
		st.Problems = append(st.Problems, problem{"piggery is loaded twice: " + copyDir + " and " + strings.Join(entries, ", ") + " in settings.json", fix})
	case len(entries) > 1:
		st.Problems = append(st.Problems, problem{"piggery's extension is listed more than once in " + path, fix})
	}
	switch {
	case managed:
		st.Installed, st.Detail = true, copyDir+" ("+have+")"
		if !local.PiExtCurrent(copyDir, version) {
			st.Problems = append(st.Problems, problem{fmt.Sprintf("the installed copy (%s) is not this piggery's (%s)", have, version), fix})
		}
	case len(entries) > 0:
		st.Installed, st.Detail = true, "checkout "+strings.Join(entries, ", ")
		for _, e := range entries {
			if _, err := os.Stat(e); err != nil {
				st.Problems = append(st.Problems, problem{e + " does not exist", fix})
			}
		}
	}
	if b, err := os.ReadFile(local.ProfilePath(dir)); err == nil {
		var p struct{ Args []string }
		if json.Unmarshal(b, &p) == nil {
			for i := 1; i < len(p.Args); i++ {
				if p.Args[i-1] == "-e" {
					if _, err := os.Stat(p.Args[i]); err != nil {
						st.Problems = append(st.Problems, problem{"pi workers load " + p.Args[i] + ", which does not exist", fix})
					}
				}
			}
		}
	}
	if !st.Installed {
		return st
	}
	if p, err := exec.LookPath("piggery"); err != nil {
		st.Problems = append(st.Problems, problem{"`piggery` is not on PATH: the extension cannot start the daemon", "put " + self + " on PATH as piggery"})
	} else if r, _ := filepath.EvalSymlinks(p); r != self {
		st.Problems = append(st.Problems, problem{fmt.Sprintf("`piggery` on PATH is %s, not %s: the extension starts that one", p, self), "put " + self + " first on PATH as piggery"})
	}
	return st
}
