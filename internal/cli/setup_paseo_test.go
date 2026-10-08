package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakePaseo keeps the daemon's plugins in <log>.state and writes the argv of every command but
// `plugin ls` to log. `--home H` must come last, as setup passes it.
func fakePaseo(log string, args []string) int {
	statePath := log + ".state"
	var plugins []paseoPlugin
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &plugins)
	}
	if n := len(args); n >= 2 && args[n-2] == "--home" {
		args = args[:n-2]
	}
	switch strings.Join(args[:min(2, len(args))], " ") {
	case "plugin ls":
		b, _ := json.Marshal(plugins)
		os.Stdout.Write(b)
		return 0
	case "plugin install":
		if len(plugins) > 0 {
			os.Stdout.WriteString(`{"error":{"code":"X","message":"Plugin ID \"piggery\" is already configured"}}`)
			return 1
		}
		plugins = append(plugins, paseoPlugin{ID: paseoPluginID, Path: args[2], Status: "running", Enabled: true})
	case "plugin enable":
		plugins[0].Enabled = true
	case "plugin reload":
	case "plugin remove":
		plugins = slices.DeleteFunc(plugins, func(p paseoPlugin) bool { return p.ID == args[2] })
	default:
		os.Stdout.WriteString(`{"error":{"code":"X","message":"fake paseo: unknown command"}}`)
		return 1
	}
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	f.WriteString(strings.Join(os.Args[1:], " ") + "\n")
	f.Close()
	b, _ := json.Marshal(plugins)
	os.WriteFile(statePath, b, 0o600)
	return 0
}

// setup paseo / setup remove paseo through Paseo's own commands, into the --paseo-home daemon:
// writes the plugin and installs it, changes nothing when run again, rewrites and reloads what an
// older piggery wrote (status says so first), enables a disabled one, replaces one from another
// directory, and removes all of it. The plugin names the binary that set it up, and a moved binary
// writes it again. Without paseo on PATH the status line is only a hint.
func TestSetupPaseoInstallRemove(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	linkExe(t, bin, "paseo")
	t.Setenv("PATH", bin)
	log := filepath.Join(t.TempDir(), "argv")
	t.Setenv("PIGGERY_FAKE_PASEO", log)
	o := setupOpts{dir: dir, self: "/opt/a/piggery", paseoHome: "/h"}
	dst := paseoDir(dir)
	install := "plugin install " + dst + " --home /h"

	if _, err := installPaseo(o); err != nil {
		t.Fatal(err)
	}
	if got := changes(t, log); !slices.Equal(got, []string{install}) {
		t.Fatalf("install ran %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "paseo-plugin.json")); err != nil || !strings.Contains(string(b), `"id": "`+paseoPluginID+`"`) {
		t.Fatalf("plugin not written: %v %s", err, b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, paseoInstalled)); string(b) != "export const piggeryPath = \"/opt/a/piggery\";\n" {
		t.Fatalf("%s: %q", paseoInstalled, b)
	}
	if msg, err := installPaseo(o); err != nil || !strings.Contains(msg, "already installed") || changes(t, log) != nil {
		t.Fatalf("second run: %q %v", msg, err)
	}
	if st := paseoStatus(o); !st.Installed || len(st.Problems) != 0 {
		t.Fatalf("status %+v", st)
	}

	o.self = "/opt/b/piggery"
	if st := paseoStatus(o); len(st.Problems) != 1 {
		t.Fatalf("moved binary: %+v", st.Problems)
	}
	if _, err := installPaseo(o); err != nil || !slices.Equal(changes(t, log), []string{"plugin reload piggery --home /h"}) {
		t.Fatalf("moved binary not written again: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, paseoInstalled)); !strings.Contains(string(b), `"/opt/b/piggery"`) {
		t.Fatalf("%s: %q", paseoInstalled, b)
	}

	os.WriteFile(filepath.Join(dst, "VERSION"), []byte("0.0.1\n"), 0o644)
	if st := paseoStatus(o); len(st.Problems) != 1 || st.Problems[0].Fix != "piggery setup --outdated --paseo-home /h" {
		t.Fatalf("stale copy: %+v", st.Problems)
	}
	if _, err := installPaseo(o); err != nil || !slices.Equal(changes(t, log), []string{"plugin reload piggery --home /h"}) {
		t.Fatalf("stale copy not reloaded: %v", err)
	}
	for _, c := range []struct {
		have paseoPlugin
		want []string
	}{
		{paseoPlugin{ID: paseoPluginID, Path: dst}, []string{"plugin enable piggery --home /h"}},
		{paseoPlugin{ID: paseoPluginID, Path: "/checkout", Enabled: true}, []string{"plugin remove piggery --home /h", install}},
	} {
		b, _ := json.Marshal([]paseoPlugin{c.have})
		os.WriteFile(log+".state", b, 0o600)
		if _, err := installPaseo(o); err != nil || !slices.Equal(changes(t, log), c.want) {
			t.Fatalf("from %+v: %v, want %q", c.have, err, c.want)
		}
	}

	if _, err := removePaseo(o); err != nil {
		t.Fatal(err)
	}
	if got := changes(t, log); !slices.Equal(got, []string{"plugin remove piggery --home /h"}) {
		t.Fatalf("remove ran %q", got)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("%s left: %v", dst, err)
	}

	t.Setenv("PATH", t.TempDir())
	if st := paseoStatus(o); st.Installed || len(st.Problems) != 0 {
		t.Fatalf("without paseo: %+v", st)
	}
}
