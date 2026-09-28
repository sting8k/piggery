package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sting8k/piggery/internal/core"
	"github.com/sting8k/piggery/internal/driver/local"
	"github.com/sting8k/piggery/manifests"
)

// Filled is a config file EnsureFiles changed, and the keys it added.
type Filled struct {
	Path  string
	Added []string
}

// EnsureFiles gives every config file piggery owns under dir the keys it lacks, with their
// defaults: config.yaml (EnsureConfig), the worker profiles that exist, and every template
// (built-in, edited or the user's; an unedited built-in already has every key). Nothing a file has
// changes; a file its parser refuses is left as it is and named in errs. `piggery setup` and the
// daemon's start run it; the daemon only logs errs.
func EnsureFiles(dir string) (filled []Filled, errs []error) {
	added, manual, err := EnsureConfig(dir)
	switch {
	case err != nil:
		errs = append(errs, err)
	case len(added) > 0:
		filled = append(filled, Filled{ConfigPath(dir), added})
	}
	if len(manual) > 0 {
		errs = append(errs, fmt.Errorf("%s: add %s by hand (gc is not an indented block)", ConfigPath(dir), strings.Join(manual, ", ")))
	}
	for _, p := range local.DefaultProfiles(dir) {
		if added, err := local.FillProfile(p.Path, p.Default); err != nil {
			errs = append(errs, err)
		} else if len(added) > 0 {
			filled = append(filled, Filled{p.Path, added})
		}
	}
	paths, _ := filepath.Glob(filepath.Join(manifests.Dir(dir), "*", manifests.ManifestFile))
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if added, err := core.FillManifestFile(p); err != nil {
			errs = append(errs, err)
		} else if len(added) > 0 {
			filled = append(filled, Filled{p, added})
		}
	}
	return filled, errs
}
