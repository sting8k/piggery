// Package manifests keeps the team templates: their home (~/.piggery/templates), the built-ins
// embedded here (this directory's *.yaml and prompts) unpacked into it, and making a manifest
// self-contained (instructions_file inlined).
package manifests

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed *.yaml prompts rules
var builtin embed.FS

// Templates live in one place, <home>/templates/<name>/:
// manifest.yaml and the prompt files it references. found, templates and team up <name> look
// only there. The built-ins embedded here are unpacked there (Unpack) and edited in place.

// Dir is where templates live under the piggery dir home.
func Dir(home string) string { return filepath.Join(home, "templates") }

// ManifestFile is a template's manifest inside its directory.
const ManifestFile = "manifest.yaml"

// recordFile keeps, per built-in template, the hash of each file Unpack wrote.
const recordFile = ".builtin.json"

func validName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid template name %q", name)
	}
	return nil
}

// Resolve returns template name from <home>/templates/<name>/manifest.yaml as a
// self-contained manifest. No such template -> an error wrapping fs.ErrNotExist.
func Resolve(name, home string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	p := filepath.Join(Dir(home), name, ManifestFile)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no template %q in %s: %w", name, Dir(home), fs.ErrNotExist)
		}
		return "", err
	}
	return Inline(p)
}

// Listed is a template in the home and its directory.
type Listed struct{ Name, From string }

// List returns the templates in <home>/templates, by name: every directory with a manifest.
func List(home string) ([]Listed, error) {
	entries, err := os.ReadDir(Dir(home))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Listed
	for _, e := range entries {
		dir := filepath.Join(Dir(home), e.Name())
		if validName(e.Name()) != nil || !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
			out = append(out, Listed{Name: e.Name(), From: dir})
		}
	}
	return out, nil
}

// Builtin returns the built-in template name as a self-contained manifest (the solo limits
// come from p2p); none -> an error wrapping fs.ErrNotExist. found never uses it: it reads the home.
func Builtin(name string) (string, error) {
	p := name + ".yaml"
	raw, err := builtin.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", name, fs.ErrNotExist)
	}
	return inline(p, raw, func(file string) ([]byte, error) {
		if path.IsAbs(file) {
			return nil, fmt.Errorf("built-in template: absolute instructions_file %s", file)
		}
		return builtin.ReadFile(path.Join(path.Dir(p), file))
	})
}

// Builtins names the built-in templates.
func Builtins() []string {
	names, _ := builtinFiles(builtin)
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// builtinFiles maps each built-in template in src (<name>.yaml at its root) to its files, as
// written in a template directory: manifest.yaml and each instructions_file (relative path).
func builtinFiles(src fs.FS) (map[string]map[string][]byte, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]byte{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(src, e.Name())
		if err != nil {
			return nil, err
		}
		files := map[string][]byte{ManifestFile: raw}
		var m struct {
			Roles map[string]struct {
				File string `yaml:"instructions_file"`
			} `yaml:"roles"`
		}
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("built-in %s: %w", name, err)
		}
		for _, r := range m.Roles {
			if r.File == "" {
				continue
			}
			if path.IsAbs(r.File) || strings.HasPrefix(path.Clean(r.File), "..") {
				return nil, fmt.Errorf("built-in %s: instructions_file %s must be inside the template", name, r.File)
			}
			b, err := fs.ReadFile(src, r.File)
			if err != nil {
				return nil, fmt.Errorf("built-in %s: %w", name, err)
			}
			files[path.Clean(r.File)] = b
		}
		out[name] = files
	}
	return out, nil
}

// Unpack brings the built-in templates into <home>/templates. A template seen for the first
// time is written whole. After that, for a template still there: a file whose content still
// has the hash Unpack recorded is updated to the current version; a file the user changed or
// removed is left alone; a file new in this version is added. A built-in template whose
// directory the user removed is not written again. A same-named template the user made
// before its built-in existed is left alone.
func Unpack(home string) error { return unpack(home, builtin) }

func unpack(home string, src fs.FS) error {
	files, err := builtinFiles(src)
	if err != nil {
		return err
	}
	root := Dir(home)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	record := map[string]map[string]string{} // template -> file -> sha256 written
	if b, err := os.ReadFile(filepath.Join(root, recordFile)); err == nil {
		if err := json.Unmarshal(b, &record); err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(root, recordFile), err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for name, tf := range files {
		dir := filepath.Join(root, name)
		rec, known := record[name]
		_, statErr := os.Stat(dir)
		switch {
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			return statErr
		case statErr != nil && known: // the user removed it
			continue
		case statErr == nil && !known: // the user's own template of that name
			continue
		case !known:
			rec = map[string]string{}
			record[name] = rec
		}
		for rel, content := range tf {
			if err := syncFile(filepath.Join(dir, filepath.FromSlash(rel)), content, rec, rel); err != nil {
				return err
			}
		}
	}
	return writeRecord(filepath.Join(root, recordFile), record)
}

// syncFile brings built-in file p (key in rec, the hashes Unpack wrote) to content: written when
// missing and never written, updated while it still has the hash recorded; a file the user
// changed or removed, or one that was never ours, is left alone.
func syncFile(p string, content []byte, rec map[string]string, key string) error {
	want := hash(content)
	cur, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, written := rec[key]; written {
			return nil // written before, removed by the user
		}
	case err != nil:
		return err
	case hash(cur) == want:
		rec[key] = want // already this version (also when edited to it by hand): ours again
		return nil
	case hash(cur) != rec[key]:
		return nil // changed by the user (or not ours)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, content, 0o600); err != nil {
		return err
	}
	rec[key] = want
	return nil
}

func writeRecord(p string, record any) error {
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// BuiltinPrompt is a shared prompt piggery ships: File (relative to the piggery dir, as a
// config.yaml `prompts` entry names it) for Roles.
type BuiltinPrompt struct {
	File  string
	Roles []string
}

// BuiltinPrompts are the shared prompts piggery ships, for the roles of its built-in templates.
var BuiltinPrompts = []BuiltinPrompt{{File: "rules/general-policy.md", Roles: []string{"lead-peer/*", "slp/*"}}}

// UnpackRules brings the BuiltinPrompts files into <home>/rules the way Unpack does a template's
// files (<home>/rules/.builtin.json records them). taken are the prompts that became piggery's in
// this call (written, or found with the same text): the caller adds their config entry once, so
// an entry or a file the user removed later is not brought back.
func UnpackRules(home string) (taken []BuiltinPrompt, err error) {
	recPath := filepath.Join(home, "rules", recordFile)
	rec := map[string]string{}
	if b, err := os.ReadFile(recPath); err == nil {
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", recPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, bp := range BuiltinPrompts {
		content, err := builtin.ReadFile(bp.File)
		if err != nil {
			return nil, err
		}
		_, known := rec[bp.File]
		if err := syncFile(filepath.Join(home, filepath.FromSlash(bp.File)), content, rec, bp.File); err != nil {
			return nil, err
		}
		if _, now := rec[bp.File]; now && !known {
			taken = append(taken, bp)
		}
	}
	if len(rec) == 0 {
		return nil, nil
	}
	return taken, writeRecord(recPath, rec)
}

// New writes the current built-in from as a new template name in the home, for the user to
// edit. It refuses a name that exists; the copy is the user's (Unpack never touches it).
func New(home, name, from string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	files, err := builtinFiles(builtin)
	if err != nil {
		return "", err
	}
	tf, ok := files[from]
	if !ok {
		return "", fmt.Errorf("no built-in template %q (built-ins: %s)", from, strings.Join(Builtins(), ", "))
	}
	dir := filepath.Join(Dir(home), name)
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("template %q already exists at %s", name, dir)
		}
		return "", err
	}
	for rel, content := range tf {
		if rel == ManifestFile { // template is the team's name at team up: the copy's own name
			content = renameTemplate(content, name)
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// topTemplate is the value of the manifest's top-level template: line (comments after it are kept).
var topTemplate = regexp.MustCompile(`(?m)^(template:[ \t]*)[^\s#]+`)

// renameTemplate sets the top-level template to name, changing nothing else in the file.
func renameTemplate(manifest []byte, name string) []byte {
	loc := topTemplate.FindSubmatchIndex(manifest)
	if loc == nil {
		return manifest
	}
	out := append([]byte{}, manifest[:loc[3]]...)
	out = append(out, name...)
	return append(out, manifest[loc[1]:]...)
}

// legacyKey is the top-level model: line a manifest had before its name key became template:.
var legacyKey = regexp.MustCompile(`(?m)^model:`)

// MigrateKey renames the top-level `model:` of each manifest in the home's templates to
// `template:`, changing no other byte, and returns the manifests it changed. A manifest that has a
// `template:` already is left alone. One that is still an unedited built-in (its hash is the one
// Unpack recorded) has its record moved to the new hash, so it keeps updating like before.
func MigrateKey(home string) ([]string, error) {
	ls, err := List(home)
	if err != nil {
		return nil, err
	}
	recPath := filepath.Join(Dir(home), recordFile)
	record := map[string]map[string]string{}
	if b, err := os.ReadFile(recPath); err == nil {
		if err := json.Unmarshal(b, &record); err != nil {
			return nil, fmt.Errorf("%s: %w", recPath, err)
		}
	}
	var changed []string
	recorded := false
	for _, l := range ls {
		p := filepath.Join(l.From, ManifestFile)
		cur, err := os.ReadFile(p)
		if err != nil {
			return changed, err
		}
		if !legacyKey.Match(cur) || topTemplate.Match(cur) {
			continue
		}
		loc := legacyKey.FindIndex(cur)
		out := append(append(append([]byte{}, cur[:loc[0]]...), "template:"...), cur[loc[1]:]...)
		info, err := os.Stat(p)
		if err != nil {
			return changed, err
		}
		if err := os.WriteFile(p, out, info.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = append(changed, p)
		if rec := record[l.Name]; rec != nil && rec[ManifestFile] == hash(cur) {
			rec[ManifestFile], recorded = hash(out), true
		}
	}
	if recorded {
		b, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return changed, err
		}
		return changed, os.WriteFile(recPath, append(b, '\n'), 0o600)
	}
	return changed, nil
}

func hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Inline reads the manifest file at p and replaces each role's instructions_file (relative to
// the manifest's directory) with instructions holding the file's text, so core only ever sees
// a self-contained manifest.
func Inline(p string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return inline(p, raw, func(file string) ([]byte, error) {
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(p), file)
		}
		return os.ReadFile(file)
	})
}

// inline does Inline for manifest text raw (named p in errors), reading each
// instructions_file through read.
func inline(p string, raw []byte, read func(file string) ([]byte, error)) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("manifest %s: %w", p, err)
	}
	roles := mapValue(&doc, "roles")
	if roles == nil || roles.Kind != yaml.MappingNode {
		return string(raw), nil
	}
	changed := false
	for i := 0; i+1 < len(roles.Content); i += 2 {
		name, role := roles.Content[i].Value, roles.Content[i+1]
		if role.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(role.Content); j += 2 {
			if role.Content[j].Value != "instructions_file" {
				continue
			}
			if mapValue(role, "instructions") != nil {
				return "", fmt.Errorf("manifest %s: roles.%s has both instructions and instructions_file", p, name)
			}
			text, err := read(role.Content[j+1].Value)
			if err != nil {
				return "", fmt.Errorf("manifest %s: roles.%s.instructions_file: %w", p, name, err)
			}
			role.Content[j].Value = "instructions"
			role.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(text)}
			changed = true
		}
	}
	if !changed {
		return string(raw), nil
	}
	out, err := yaml.Marshal(&doc)
	return string(out), err
}

// mapValue returns the value node of key in a mapping (or a document holding one), or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
