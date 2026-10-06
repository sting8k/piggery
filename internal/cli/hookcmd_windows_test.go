//go:build windows

package cli

import "testing"

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"2.1.283": true, "2.1.139": true, "2.1.138": false, "2.0.999": false, "3.0.0": true, "2.1": false} {
		if got := versionAtLeast(v, claudeExecFormSince); got != want {
			t.Errorf("versionAtLeast(%s) = %v, want %v", v, got, want)
		}
	}
}
