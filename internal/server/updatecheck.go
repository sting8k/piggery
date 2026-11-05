package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The daily update check: once a day the daemon asks for the newest release tag (Config.Latest, the
// call `piggery update --check` makes) and keeps the answer in <dir>/cache/update.json, next to
// logs.json and top.json. Nothing is installed; ps, top and setup say a newer one is out.
const (
	updateEvery   = 24 * time.Hour
	updateTimeout = 5 * time.Second // the call runs on serve's one-second ticker, so it is short
)

type updateCache struct {
	CheckedAt int64  `json:"checked_at"` // unix ms of the last call, whether it worked or not
	Latest    string `json:"latest"`     // the tag it returned; a failed call leaves the previous one
}

func updateCachePath(dir string) string { return filepath.Join(dir, "cache", "update.json") }

func readUpdateCache(dir string) (c updateCache) {
	if b, err := os.ReadFile(updateCachePath(dir)); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// checkUpdate is one tick of the check: a release build with update.check on, and no call in the
// last day, asks for the latest tag. A failed call (offline, rate limited) is silent: it counts as
// today's call, so it is retried tomorrow, not on the next tick.
func (s *server) checkUpdate(ctx context.Context, now time.Time) {
	if s.latest == nil || DevBuild(s.version) {
		return
	}
	if set, err := s.settingsNow(); err != nil || !set.UpdateCheck {
		return
	}
	if s.updateAt.IsZero() {
		s.updateAt = time.UnixMilli(readUpdateCache(s.dir).CheckedAt)
	}
	if now.Sub(s.updateAt) < updateEvery {
		return
	}
	s.updateAt = now
	c := readUpdateCache(s.dir)
	c.CheckedAt = now.UnixMilli()
	cctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	if tag, err := s.latest(cctx); err == nil {
		c.Latest = tag
	}
	if b, err := json.Marshal(c); err == nil {
		if os.MkdirAll(filepath.Dir(updateCachePath(s.dir)), 0o700) == nil {
			_ = os.WriteFile(updateCachePath(s.dir), b, 0o600)
		}
	}
}

// UpdateAvailable is the cached latest tag when it is newer than current, else "". It reads the
// cache file only (no call); a dev build and update.check: false never have one.
func UpdateAvailable(dir string, set Settings, current string) string {
	if DevBuild(current) || !set.UpdateCheck {
		return ""
	}
	if tag := readUpdateCache(dir).Latest; Newer(tag, current) {
		return tag
	}
	return ""
}

// DevBuild reports whether version v is a build from source, not a release: none, "dev", or
// "dev-<sha>" (a local deploy). It never checks for, says or installs a release.
func DevBuild(v string) bool { return v == "" || v == "dev" || strings.HasPrefix(v, "dev-") }

// Newer reports whether release tag latest is newer than current: both vX.Y.Z (a pre-release
// suffix is ignored); a tag that is not that counts as newer when it differs.
func Newer(latest, current string) bool {
	a, okA := versionNumbers(latest)
	b, okB := versionNumbers(current)
	if !okA || !okB {
		return latest != "" && latest != current
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func versionNumbers(tag string) (v [3]int, ok bool) {
	tag, _, _ = strings.Cut(strings.TrimPrefix(tag, "v"), "-")
	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
