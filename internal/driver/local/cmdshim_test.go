package local

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// npmShim is the .cmd npm's cmd-shim (lib/index.js, writeShim_) writes for a script run by prog.
func npmShim(prog, target string) string {
	return "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n\r\n" +
		"IF EXIST \"%dp0%\\" + prog + ".exe\" (\r\n  SET \"_prog=%dp0%\\" + prog + ".exe\"\r\n) ELSE (\r\n  SET \"_prog=" + prog + "\"\r\n)\r\n\r\n" +
		"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & set PATHEXT=%PATHEXT:;.JS;=;% & \"%_prog%\"  \"%dp0%\\" + target + "\" %*\r\n"
}

// onPath puts an empty program called name in a new directory and makes that the PATH.
func onPath(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(p))
	return p
}

func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An npm shim runs its script with the interpreter the script's shebang names, which is not always
// node (omp's is bun): the one beside the shim when there is one, else the one on PATH.
func TestNpmShimRunsItsOwnInterpreter(t *testing.T) {
	dir := t.TempDir()
	script := touch(t, filepath.Join(dir, "node_modules", "pkg", "cli.js"))
	bun := onPath(t, "bun")
	shim := []byte(npmShim("bun", `node_modules\pkg\cli.js`))
	if path, pre, ok := resolveShim(shim, dir); !ok || path != bun || !slices.Equal(pre, []string{script}) {
		t.Fatalf("resolveShim = %q %q %v; want bun from PATH running %s", path, pre, ok, script)
	}
	beside := touch(t, filepath.Join(dir, "bun.exe"))
	if path, _, ok := resolveShim(shim, dir); !ok || path != beside {
		t.Fatalf("resolveShim = %q %v; want the bun.exe beside the shim", path, ok)
	}
}

// A pnpm shim (@zkochan/cmd-shim, generateCmdShim) names its directory %~dp0 and writes the run line
// twice, for the interpreter beside the shim and for the one on PATH; what stands between the
// interpreter and the script is passed on.
func TestPnpmShimIsSeenThrough(t *testing.T) {
	dir := t.TempDir()
	script := touch(t, filepath.Join(dir, "global", "5", "node_modules", "pkg", "bin", "cli.js"))
	loader := touch(t, filepath.Join(dir, "loader.js"))
	node := onPath(t, "node")
	target := `"%~dp0\global\5\node_modules\pkg\bin\cli.js"`
	shim := "@SETLOCAL\r\n@IF NOT DEFINED NODE_PATH (\r\n  @SET \"NODE_PATH=C:\\x\\node_modules\"\r\n) ELSE (\r\n  @SET \"NODE_PATH=C:\\x\\node_modules;%NODE_PATH%\"\r\n)\r\n" +
		"@IF EXIST \"%~dp0\\node.exe\" (\r\n  \"%~dp0\\node.exe\" --require \"%~dp0\\loader.js\" " + target + " %*\r\n" +
		") ELSE (\r\n  @SET PATHEXT=%PATHEXT:;.JS;=;%\r\n  node --require \"%~dp0\\loader.js\" " + target + " %*\r\n)\r\n"
	path, pre, ok := resolveShim([]byte(shim), dir)
	if want := []string{"--require", loader, script}; !ok || path != node || !slices.Equal(pre, want) {
		t.Fatalf("resolveShim = %q %q %v; want %s %q", path, pre, ok, node, want)
	}
}

// A shim whose target is a program is that program. A file that is no shim of these forms is left
// to cmd.exe: a program named by an absolute path, a line with a variable only cmd.exe can expand,
// and a script whose interpreter is not installed.
func TestShimOfAProgramAndFormsLeftToCmd(t *testing.T) {
	dir := t.TempDir()
	exe := touch(t, filepath.Join(dir, "node_modules", "@openai", "codex", "bin", "codex.exe"))
	head := "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\n"
	if path, pre, ok := resolveShim([]byte(head+"\"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.exe\"   %*\r\n"), dir); !ok || path != exe || len(pre) != 0 {
		t.Fatalf("resolveShim = %q %q %v; want %s alone", path, pre, ok, exe)
	}
	touch(t, filepath.Join(dir, "cli.js"))
	onPath(t, "node")
	for name, shim := range map[string]string{
		"absolute program":      "@\"" + exe + "\" %*\r\n",
		"variable in a flag":    "node --max-old-space-size=%MEM% \"%~dp0\\cli.js\" %*\r\n",
		"interpreter not there": npmShim("deno", "cli.js"),
		"script not there":      npmShim("node", "gone.js"),
	} {
		if path, pre, ok := resolveShim([]byte(shim), dir); ok {
			t.Errorf("%s: seen through as %q %q; want it left to cmd.exe", name, path, pre)
		}
	}
}
