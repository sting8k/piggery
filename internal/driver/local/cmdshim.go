package local

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// A harness installed with npm or pnpm (pi, claude, codex, omp, opencode) is a .cmd shim on
// Windows. This file reads what such a shim runs; shim_windows.go is its one caller. It has no
// build tag because it only reads text and looks files up: the tests of every OS read real shims.
//
// A shim starts its program on a line that ends in %*, and names its own directory %dp0% (npm's
// cmd-shim) or %~dp0 (pnpm's cmd-shim, and npm before 7):
//
//	... & "%_prog%" [flags] "%dp0%\path\to\cli.js" %*      npm: an interpreter, one of SET "_prog=..."
//	"%dp0%\path\to\tool.exe" %*                            npm: a program
//	"%~dp0\node.exe" [flags] "%~dp0\path\to\cli.js" %*     pnpm: the interpreter beside the shim,
//	node [flags] "%~dp0\path\to\cli.js" %*                 or else the one on PATH
//	@"%~dp0\path\to\tool.exe" %*                           pnpm: a program
//
// The interpreter is whatever the script's shebang names (node, bun, ...), so it is read from the
// shim, never assumed.

// shimRun is one line of a shim that starts its program: prog runs target with flags before it and
// the caller's arguments after. prog is "" when target is the program itself.
type shimRun struct {
	prog   string // "%_prog%", a path under the shim's directory, a path, or a name on PATH
	flags  []string
	target string // below the shim's directory, as the shim writes it (\ between names)
}

var shimProgSet = regexp.MustCompile(`(?mi)^[ \t]*@?SET[ \t]+"_prog=([^"\r\n]+)"`)

// underShimDir is s without the shim's name for its own directory in front of it.
func underShimDir(s string) (rel string, ok bool) {
	for _, d := range []string{`%dp0%\`, `%~dp0\`} {
		if len(s) > len(d) && strings.EqualFold(s[:len(d)], d) {
			return s[len(d):], true
		}
	}
	return "", false
}

// shimWords splits s at blanks, taking "a b" as one word (without its quotes); ok false when a
// quote is not closed.
func shimWords(s string) (words []string, quoted []bool, ok bool) {
	for s = strings.TrimLeft(s, " \t"); s != ""; s = strings.TrimLeft(s, " \t") {
		if s[0] == '"' {
			i := strings.IndexByte(s[1:], '"')
			if i < 0 {
				return nil, nil, false
			}
			words, quoted, s = append(words, s[1:1+i]), append(quoted, true), s[i+2:]
			continue
		}
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			i = len(s)
		}
		words, quoted, s = append(words, s[:i]), append(quoted, false), s[i:]
	}
	return words, quoted, true
}

// lastCommand is what follows the last & of line that is not inside quotes: the command a line of
// several (a & b & c) ends with.
func lastCommand(line string) string {
	inQuote, from := false, 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '&':
			if !inQuote {
				from = i + 1
			}
		}
	}
	return line[from:]
}

// parseShim reads the lines of shim b that start its program, in the file's order, and the
// interpreters its SET "_prog=..." lines name, in theirs.
func parseShim(b []byte) (runs []shimRun, progs []string) {
	for _, m := range shimProgSet.FindAllSubmatch(b, -1) {
		progs = append(progs, string(m[1]))
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "%*") {
			continue
		}
		words, quoted, ok := shimWords(strings.TrimPrefix(strings.TrimSpace(lastCommand(line)), "@"))
		n := len(words)
		if !ok || n < 2 || words[n-1] != "%*" || quoted[n-1] || !quoted[n-2] {
			continue
		}
		target, ok := underShimDir(words[n-2])
		if !ok {
			continue
		}
		run := shimRun{target: target}
		if n > 2 {
			run.prog, run.flags = words[0], words[1:n-2]
		}
		runs = append(runs, run)
	}
	return runs, progs
}

// resolveShim is the program the shim b of directory dir runs, and the arguments it gives it before
// the caller's: the first of its lines whose program and target are both there. ok false when the
// shim is of another form, or what it names cannot be found.
func resolveShim(b []byte, dir string) (path string, pre []string, ok bool) {
	runs, progs := parseShim(b)
	below := func(rel string) string {
		return filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
	}
	isFile := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir()
	}
	var interpreter func(w string) string
	interpreter = func(w string) string {
		p := ""
		switch rel, under := underShimDir(w); {
		case w == "%_prog%":
			for _, name := range progs {
				if name != w {
					if p = interpreter(name); p != "" {
						break
					}
				}
			}
			return p
		case under:
			p = below(rel)
		case strings.Contains(w, "%"):
			return "" // a variable only cmd.exe can expand
		case strings.ContainsAny(w, `\/`):
			p = w
		default:
			found, err := exec.LookPath(w)
			if err != nil { // not on PATH, or only in the current directory (exec.ErrDot)
				return ""
			}
			p = found
		}
		if ext := strings.ToLower(filepath.Ext(p)); p == "" || !isFile(p) || ext == ".cmd" || ext == ".bat" {
			return "" // not there, or itself a batch file: cmd.exe would read the arguments again
		}
		return p
	}
	for _, r := range runs {
		target := below(r.target)
		if !isFile(target) {
			continue
		}
		if r.prog == "" {
			if strings.EqualFold(filepath.Ext(target), ".exe") {
				return target, nil, true
			}
			continue
		}
		prog := interpreter(r.prog)
		if prog == "" {
			continue
		}
		pre, plain := make([]string, 0, len(r.flags)+1), true
		for _, f := range r.flags {
			if rel, under := underShimDir(f); under {
				f = below(rel)
			}
			plain = plain && !strings.Contains(f, "%")
			pre = append(pre, f)
		}
		if plain {
			return prog, append(pre, target), true
		}
	}
	return "", nil, false
}
