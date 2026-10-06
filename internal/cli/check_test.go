package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sting8k/piggery/internal/server"
	"github.com/sting8k/piggery/manifests"
)

// `piggery check` through the real entrypoint, with no daemon: one template team up would refuse is
// one error, one prompts entry the daemon would skip is one warning, a good template says nothing,
// and the exit is 1; with the error gone it is 0 and the warning stays.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	put := func(path, text string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(manifests.Dir(dir), "good", manifests.ManifestFile), "template: good\nroles:\n  lead:\n    instructions: \"Lead.\"\n")
	bad := filepath.Join(manifests.Dir(dir), "bad", manifests.ManifestFile)
	put(bad, "roles: {}\n")
	put(server.ConfigPath(dir), "prompts:\n  - {file: nope.md, roles: [solo]}\n")
	check := func() (int, checkReport) {
		var out, errOut bytes.Buffer
		code := Main(dir, []string{"--json", "check"}, &out, &errOut)
		var r checkReport
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("exit %d: %v\n%s%s", code, err, out.String(), errOut.String())
		}
		// the built-in templates are the binary's, not this dir's: their lines are not asserted
		for _, l := range []*[]string{&r.Errors, &r.Warnings} {
			*l = slices.DeleteFunc(*l, func(s string) bool { return !strings.HasPrefix(s, dir) })
		}
		return code, r
	}
	code, r := check()
	if code != 1 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], bad) || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "nope.md") {
		t.Fatalf("exit %d, %+v; want one error (bad template), one warning (nope.md)", code, r)
	}
	os.RemoveAll(filepath.Dir(bad))
	if code, r := check(); code != 0 || len(r.Errors) != 0 || len(r.Warnings) != 1 {
		t.Fatalf("without the bad template: exit %d, %+v", code, r)
	}
}

// `piggery template list` reads the home as team up does: before the first setup it lists the
// built-ins the daemon will unpack; then each template with its summary and where it comes from.
func TestTemplateList(t *testing.T) {
	dir := t.TempDir()
	list := func() string {
		var out, errOut bytes.Buffer
		if code := Main(dir, []string{"template", "list"}, &out, &errOut); code != 0 {
			t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
		}
		return out.String()
	}
	if got := list(); !strings.Contains(got, "built-in") || strings.Contains(got, "yours") {
		t.Fatalf("empty home:\n%s", got)
	} else if !regexp.MustCompile(`(?m)^council .* taskforce `).MatchString(got) || regexp.MustCompile(`(?m)^lead-peer .* taskforce `).MatchString(got) {
		t.Fatalf("council has a taskforce: block, lead-peer has none:\n%s", got)
	}
	p := filepath.Join(manifests.Dir(dir), "mine", manifests.ManifestFile)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("template: mine\nsummary: my  own\n  team\nroles:\n  lead:\n    instructions: \"Lead.\"\n"), 0o600)
	if got := list(); !strings.Contains(got, "yours") || !strings.Contains(got, "my own team") || strings.Contains(got, "built-in") {
		t.Fatalf("home with one template:\n%s", got)
	}
}
