package core

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gitTimeout bounds `git worktree list` (spawn with a cwd outside the team root).
const gitTimeout = 3 * time.Second

// spawnDir is a spawn's cwd: relative to from (the spawner's cwd) or absolute, an existing
// directory, symlinks resolved.
func spawnDir(from, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(from, cwd)
	}
	return normalizeCwd(cwd)
}

// inBounds reports whether dir (symlinks resolved) is inside root, inside a git worktree of the
// repo at root other than root's own checkout, or inside an allowed root: the engine's bounds,
// which no template widens.
func (e *Engine) inBounds(ctx context.Context, root, dir string) bool {
	if within(root, dir) {
		return true
	}
	for _, a := range e.allowedRoots {
		if r, err := filepath.EvalSymlinks(a); err == nil && within(r, dir) {
			return true
		}
	}
	return slices.ContainsFunc(worktrees(ctx, root), func(w string) bool { return !within(w, root) && within(w, dir) })
}

// worktrees are the worktrees of the git repo at root, symlinks resolved; none when root is not
// in a repo or git fails or takes longer than gitTimeout.
func worktrees(ctx context.Context, root string) []string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "list", "--porcelain")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var dirs []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "worktree "); ok {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				dirs = append(dirs, r)
			}
		}
	}
	return dirs
}

// cwdAndRoot reads participant id's cwd and its team's root ("" for a solo).
func (t *txn) cwdAndRoot(id string, cwd, root *string) error {
	return internal(t.QueryRowContext(t.ctx, `SELECT p.cwd, COALESCE(t.root_cwd,'') FROM participants p
		LEFT JOIN teams t ON t.id=p.team_id WHERE p.id=?`, id).Scan(cwd, root))
}

func outOfBounds(dir string) string {
	return dir + " is outside the team root, the git worktrees of its repo and spawn.allowed_roots"
}

// within reports whether p is base or inside it (both clean and absolute).
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
