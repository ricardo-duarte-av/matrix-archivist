// Package gitops wraps the git CLI for mirroring repositories.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/live"
)

// Refs maps ref names (refs/heads/main, refs/tags/v1) to object IDs.
type Refs map[string]string

// Change describes what changed between two ref snapshots.
type Change struct {
	NewBranches     []string
	UpdatedBranches []string
	DeletedBranches []string
	NewTags         []string
	DeletedTags     []string
	NewCommits      int // commits reachable from updated/new branches that weren't before (approximate)

	// RewrittenBranches were updated in a way that dropped commits (force push / history rewrite).
	RewrittenBranches []string `json:",omitempty"`
	// MovedTags point at a different object than before.
	MovedTags []string `json:",omitempty"`
	// Snapshot is the refs/archive/<stamp> namespace where the previous tips of rewritten,
	// deleted or moved refs were preserved ("" if nothing needed preserving).
	Snapshot string `json:",omitempty"`
}

func (c Change) Empty() bool {
	return len(c.NewBranches)+len(c.UpdatedBranches)+len(c.DeletedBranches)+len(c.NewTags)+len(c.DeletedTags)+len(c.MovedTags) == 0
}

// Destructive reports whether upstream removed or rewrote anything.
func (c Change) Destructive() bool {
	return len(c.RewrittenBranches)+len(c.DeletedBranches)+len(c.MovedTags)+len(c.DeletedTags) > 0
}

// Summary is a short human-readable description, e.g. "12 commits, 2 new tags (v1.2, v1.3)".
func (c Change) Summary() string {
	var parts []string
	if c.NewCommits > 0 {
		parts = append(parts, plural(c.NewCommits, "new commit"))
	}
	if n := len(c.UpdatedBranches); n > 0 {
		parts = append(parts, plural(n, "branch updated", "branches updated"))
	}
	if n := len(c.NewBranches); n > 0 {
		parts = append(parts, plural(n, "new branch", "new branches"))
	}
	if n := len(c.DeletedBranches); n > 0 {
		parts = append(parts, plural(n, "branch deleted", "branches deleted"))
	}
	if n := len(c.NewTags); n > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s)", plural(n, "new tag"), abbrevList(c.NewTags, 5)))
	}
	if n := len(c.DeletedTags); n > 0 {
		parts = append(parts, plural(n, "tag deleted", "tags deleted"))
	}
	if n := len(c.RewrittenBranches); n > 0 {
		parts = append(parts, fmt.Sprintf("⚠️ history rewritten on %s", abbrevList(c.RewrittenBranches, 5)))
	}
	if n := len(c.MovedTags); n > 0 {
		parts = append(parts, fmt.Sprintf("⚠️ %s (%s)", plural(n, "tag moved", "tags moved"), abbrevList(c.MovedTags, 5)))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one string, many ...string) string {
	if n == 1 {
		return "1 " + one
	}
	if len(many) > 0 {
		return fmt.Sprintf("%d %s", n, many[0])
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func abbrevList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", +%d more", len(items)-max)
}

// Git runs git commands with a hardened environment.
type Git struct {
	Binary string
	// AllowLocalRemotes permits file:// / local-path remotes. Only for tests.
	AllowLocalRemotes bool
}

func New() *Git { return &Git{Binary: "git"} }

func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	return g.runInput(ctx, dir, "", args...)
}

func (g *Git) runInput(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	return g.exec(ctx, dir, stdin, nil, args...)
}

// runProgress runs a clone/fetch with --progress and reports its progress to the live slot
// attached to ctx (if any).
func (g *Git) runProgress(ctx context.Context, dir string, args ...string) (string, error) {
	slot := live.From(ctx)
	if slot == nil {
		return g.exec(ctx, dir, "", nil, args...)
	}
	return g.exec(ctx, dir, "", func(line string) {
		if p, ok := ParseProgress(line); ok {
			slot.SetDetail(p.Text())
			slot.Progress(p.Done, p.Total, "objects")
		}
	}, args...)
}

func (g *Git) exec(ctx context.Context, dir, stdin string, onProgress func(string), args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.Binary, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Dir = dir
	killTree(cmd)
	// Don't wait forever for output pipes held open by a straggling process.
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new",
		"LC_ALL=C",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if onProgress != nil {
		cmd.Stderr = &progressWriter{onLine: onProgress, rest: &stderr}
	}
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return "", fmt.Errorf("git %s: %v: %s", firstNonFlag(args), err, msg)
	}
	return stdout.String(), nil
}

func firstNonFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "-C" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return "command"
}

// remoteSafety returns config flags applied to operations that talk to untrusted remotes.
func (g *Git) remoteSafety() []string {
	if g.AllowLocalRemotes {
		return []string{"-c", "protocol.ext.allow=never"}
	}
	return []string{"-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never"}
}

// CloneMirror creates a bare archive clone at dir (must not exist) tracking all branches
// and tags. Unlike `git clone --mirror`, forge-internal refs (refs/pull/*, refs/merge-requests/*)
// are not fetched.
func (g *Git) CloneMirror(ctx context.Context, remoteURL, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	args := append(g.remoteSafety(), "clone", "--bare", "--progress", "--", remoteURL, tmp)
	if _, err := g.runProgress(ctx, "", args...); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	for _, a := range [][]string{
		{"config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"},
	} {
		if _, err := g.run(ctx, tmp, a...); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	return os.Rename(tmp, dir)
}

// FetchMirror updates an archive clone from its remote. With prune, branches/tags deleted
// upstream are deleted locally too; without it they are kept (safer for archiving).
// It also follows changes of the remote's default branch.
func (g *Git) FetchMirror(ctx context.Context, dir string, prune bool) error {
	// No automatic gc during the fetch: objects that upstream dropped must still exist when
	// PreserveRefs runs right after.
	args := append(g.remoteSafety(), "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "--progress", "--force")
	if prune {
		args = append(args, "--prune", "--prune-tags")
	}
	args = append(args, "origin")
	if _, err := g.runProgress(ctx, dir, args...); err != nil {
		return err
	}
	// Best effort: track upstream default branch.
	lsArgs := append(g.remoteSafety(), "ls-remote", "--symref", "origin", "HEAD")
	if out, err := g.run(ctx, dir, lsArgs...); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: "); ok {
				if ref, _, ok := strings.Cut(rest, "\t"); ok && strings.HasPrefix(ref, "refs/heads/") {
					if _, err := g.run(ctx, dir, "rev-parse", "--verify", "-q", ref); err == nil {
						_, _ = g.run(ctx, dir, "symbolic-ref", "HEAD", ref)
					}
				}
			}
		}
	}
	return nil
}

// SnapshotRefs lists branches and tags of a repository.
func (g *Git) SnapshotRefs(ctx context.Context, dir string) (Refs, error) {
	out, err := g.run(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	refs := Refs{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if obj, name, ok := strings.Cut(line, " "); ok {
			refs[name] = obj
		}
	}
	return refs, nil
}

// Diff compares two snapshots and counts new commits on branches.
func (g *Git) Diff(ctx context.Context, dir string, before, after Refs) Change {
	var c Change
	var oldTips, newTips []string
	for name, obj := range after {
		old, existed := before[name]
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			short := strings.TrimPrefix(name, "refs/heads/")
			if !existed {
				c.NewBranches = append(c.NewBranches, short)
				newTips = append(newTips, obj)
			} else if old != obj {
				c.UpdatedBranches = append(c.UpdatedBranches, short)
				newTips = append(newTips, obj)
			}
		case strings.HasPrefix(name, "refs/tags/"):
			short := strings.TrimPrefix(name, "refs/tags/")
			if !existed {
				c.NewTags = append(c.NewTags, short)
			} else if old != obj {
				c.MovedTags = append(c.MovedTags, short)
			}
		}
	}
	for name, obj := range before {
		if _, ok := after[name]; ok {
			if strings.HasPrefix(name, "refs/heads/") {
				oldTips = append(oldTips, obj)
			}
			continue
		}
		if strings.HasPrefix(name, "refs/heads/") {
			c.DeletedBranches = append(c.DeletedBranches, strings.TrimPrefix(name, "refs/heads/"))
			oldTips = append(oldTips, obj)
		} else if strings.HasPrefix(name, "refs/tags/") {
			c.DeletedTags = append(c.DeletedTags, strings.TrimPrefix(name, "refs/tags/"))
		}
	}
	for _, br := range c.UpdatedBranches {
		// The old tip is no longer reachable from the new one: commits were dropped.
		out, err := g.run(ctx, dir, "rev-list", "-n", "1", before["refs/heads/"+br], "^"+after["refs/heads/"+br], "--")
		if err == nil && strings.TrimSpace(out) != "" {
			c.RewrittenBranches = append(c.RewrittenBranches, br)
		}
	}
	for _, s := range [][]string{c.NewBranches, c.UpdatedBranches, c.DeletedBranches, c.NewTags, c.DeletedTags, c.RewrittenBranches, c.MovedTags} {
		sort.Strings(s)
	}
	if len(newTips) > 0 && len(before) > 0 {
		args := []string{"rev-list", "--count"}
		args = append(args, newTips...)
		for _, t := range oldTips {
			args = append(args, "^"+t)
		}
		args = append(args, "--")
		if out, err := g.run(ctx, dir, args...); err == nil {
			c.NewCommits, _ = strconv.Atoi(strings.TrimSpace(out))
		}
	}
	return c
}

// ArchiveNamespace is where PreserveRefs keeps refs that upstream rewrote or deleted.
const ArchiveNamespace = "refs/archive/"

// PreserveRefs keeps the previous tips of rewritten/deleted branches and moved/deleted tags
// under refs/archive/<stamp>/{heads,tags}/<name>, so no history is ever lost to a force push
// or deletion upstream. It sets c.Snapshot and returns the number of refs preserved.
func (g *Git) PreserveRefs(ctx context.Context, dir string, before Refs, c *Change, stamp string) (int, error) {
	var lines []string
	add := func(kind string, names []string) {
		for _, n := range names {
			if obj := before["refs/"+kind+"/"+n]; obj != "" {
				lines = append(lines, fmt.Sprintf("create %s%s/%s/%s %s", ArchiveNamespace, stamp, kind, n, obj))
			}
		}
	}
	add("heads", c.RewrittenBranches)
	add("heads", c.DeletedBranches)
	add("tags", c.MovedTags)
	add("tags", c.DeletedTags)
	if len(lines) == 0 {
		return 0, nil
	}
	if _, err := g.runInput(ctx, dir, strings.Join(lines, "\n")+"\n", "update-ref", "--stdin"); err != nil {
		return 0, err
	}
	c.Snapshot = ArchiveNamespace + stamp
	return len(lines), nil
}

// Snapshots lists the refs/archive/<stamp> namespaces (oldest first) with their ref counts.
func (g *Git) Snapshots(ctx context.Context, dir string) ([]Snapshot, error) {
	out, err := g.run(ctx, dir, "for-each-ref", "--format=%(refname)", ArchiveNamespace)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		rest, ok := strings.CutPrefix(line, ArchiveNamespace)
		if !ok {
			continue
		}
		stamp, _, _ := strings.Cut(rest, "/")
		counts[stamp]++
	}
	snaps := make([]Snapshot, 0, len(counts))
	for s, n := range counts {
		snaps = append(snaps, Snapshot{Stamp: s, Refs: n})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Stamp < snaps[j].Stamp })
	return snaps, nil
}

type Snapshot struct {
	Stamp string // e.g. 20261114T093000Z
	Refs  int
}

// Commit is a one-line commit description.
type Commit struct {
	Hash, Subject, Author string
	Time                  time.Time
}

// RecentCommits returns the latest n commits on the default branch (HEAD) of dir.
func (g *Git) RecentCommits(ctx context.Context, dir string, n int) ([]Commit, error) {
	out, err := g.run(ctx, dir, "log", "-n", strconv.Itoa(n), "--format=%h%x00%an%x00%ct%x00%s", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\x00", 4)
		if len(f) != 4 {
			continue
		}
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		commits = append(commits, Commit{Hash: f[0], Author: f[1], Time: time.Unix(ts, 0), Subject: f[3]})
	}
	return commits, nil
}

// DefaultBranch returns the branch HEAD points to in dir (short name), or "" if unknown.
func (g *Git) DefaultBranch(ctx context.Context, dir string) string {
	out, err := g.run(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// SyncCheckout creates or updates a working-tree clone of the local mirror at checkoutDir,
// tracking the mirror's default branch.
func (g *Git) SyncCheckout(ctx context.Context, mirrorDir, checkoutDir string) error {
	branch := g.DefaultBranch(ctx, mirrorDir)
	if branch == "" {
		return nil
	}
	// Mirror may be empty or HEAD may point at a missing branch.
	if _, err := g.run(ctx, mirrorDir, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err != nil {
		return nil
	}
	absMirror, err := filepath.Abs(mirrorDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(checkoutDir, ".git")); errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(checkoutDir)
		_, err := g.run(ctx, "", "clone", "--quiet", "--branch", branch, "--", absMirror, checkoutDir)
		return err
	}
	if _, err := g.run(ctx, checkoutDir, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return err
	}
	if _, err := g.run(ctx, checkoutDir, "checkout", "--quiet", "-f", "-B", branch, "origin/"+branch); err != nil {
		return err
	}
	_, err = g.run(ctx, checkoutDir, "clean", "-ffdq")
	return err
}

// DirSize returns the total size in bytes of files under dir.
func DirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// HumanSize formats bytes as e.g. "1.5 GiB".
func HumanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
