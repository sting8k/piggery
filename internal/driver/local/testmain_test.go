package local

import (
	"os"
	"os/exec"
	"testing"
)

// TestMain lets a fake harness be this test binary run with the harness's own flags first (Claude's
// flags come before the profile's args, and a test binary refuses flags it does not know): with
// PGDRV_WRAP=<test name> set, the test binary runs that test and hands it every argument after
// "--". It stands in for the shell wrapper script unix tests used, which Windows cannot run.
func TestMain(m *testing.M) {
	if name := os.Getenv("PGDRV_WRAP"); name != "" {
		os.Args = append([]string{os.Args[0], "-test.run=^" + name + "$", "--"}, os.Args[1:]...)
	}
	os.Exit(m.Run())
}

// useFakeHarness makes the profile's cmd this test binary, running test name.
func useFakeHarness(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("PGDRV_WRAP", name)
	return os.Args[0]
}

// sleeper is a process that does nothing for a long while: this test binary in the helper's
// "sleep" mode (the sleep command does not exist on Windows).
func sleeper() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "PGDRV_WRAP=", "PGDRV_HELPER=sleep")
	return cmd
}

// setHome makes dir the user's home: HOME on unix, USERPROFILE on Windows (os.UserHomeDir).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
