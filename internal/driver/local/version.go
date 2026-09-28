package local

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"time"
)

// Version drift: each harness profile lists the
// versions piggery was tested with; another installed version is worth a warning, not an error.
// `piggery setup` and doctor compare (internal/cli).

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// HarnessVersion runs `<cmd> --version` and returns the first x.y.z in its output.
func HarnessVersion(ctx context.Context, cmd string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(cctx, cmd, "--version").Output()
	if err != nil {
		return "", err
	}
	if v := versionRe.FindString(string(raw)); v != "" {
		return v, nil
	}
	return "", errors.New("no version in its --version output")
}
