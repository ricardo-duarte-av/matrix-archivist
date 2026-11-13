package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseRepoURL(t *testing.T) {
	ok := []struct{ in, key, owner, name string }{
		{"https://github.com/torvalds/linux", "github.com/torvalds/linux", "torvalds", "linux"},
		{"https://github.com/torvalds/linux.git", "github.com/torvalds/linux", "torvalds", "linux"},
		{"https://GitHub.com/Torvalds/Linux/", "github.com/torvalds/linux", "Torvalds", "Linux"},
		{"git@github.com:torvalds/linux.git", "github.com/torvalds/linux", "torvalds", "linux"},
		{"ssh://git@gitlab.com:2222/group/sub/project.git", "gitlab.com/group/sub/project", "group/sub", "project"},
		{"git://git.kernel.org/pub/scm/git/git.git", "git.kernel.org/pub/scm/git/git", "pub/scm/git", "git"},
		{"<https://codeberg.org/forgejo/forgejo>", "codeberg.org/forgejo/forgejo", "forgejo", "forgejo"},
	}
	for _, c := range ok {
		u, err := ParseRepoURL(c.in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if u.Key() != c.key || u.Owner != c.owner || u.Name != c.name {
			t.Errorf("%q: got key=%q owner=%q name=%q", c.in, u.Key(), u.Owner, u.Name)
		}
	}

	bad := []string{
		"", "file:///etc/passwd", "/tmp/repo", "../repo", "-uhttps://x.y/a",
		"https://localhost/a/b", "https://github.com/", "https://github.com/a/../b",
		"ext::sh -c touch% /tmp/pwned", "https://github.com/a/b?x=1",
		"https://gitlab.com/a/b/-/tree/main", "ftp://example.com/a",
	}
	for _, in := range bad {
		if _, err := ParseRepoURL(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commit(t *testing.T, dir, file string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-qm", file)
}

func TestMirrorLifecycle(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "upstream")
	os.MkdirAll(upstream, 0o755)
	run(t, upstream, "init", "-q", "-b", "main")
	commit(t, upstream, "a")
	// Fake a GitHub PR ref; it must not be archived.
	run(t, upstream, "update-ref", "refs/pull/1/head", "HEAD")

	g := New()
	g.AllowLocalRemotes = true
	mirror := filepath.Join(tmp, "store", "mirror.git")
	checkout := filepath.Join(tmp, "store", "checkout")

	if err := g.CloneMirror(ctx, upstream, mirror); err != nil {
		t.Fatal(err)
	}
	refs, err := g.SnapshotRefs(ctx, mirror)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs["refs/heads/main"]; !ok || len(refs) != 1 {
		t.Fatalf("unexpected refs after clone: %v", refs)
	}
	if err := g.SyncCheckout(ctx, mirror, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "a")); err != nil {
		t.Fatal("checkout missing file a")
	}

	// Upstream changes: 2 commits, a tag, a new branch.
	commit(t, upstream, "b")
	commit(t, upstream, "c")
	run(t, upstream, "tag", "v1.0")
	run(t, upstream, "branch", "feature")

	before := refs
	if err := g.FetchMirror(ctx, mirror, false); err != nil {
		t.Fatal(err)
	}
	after, _ := g.SnapshotRefs(ctx, mirror)
	ch := g.Diff(ctx, mirror, before, after)
	if ch.NewCommits != 2 || len(ch.NewTags) != 1 || len(ch.NewBranches) != 1 || len(ch.UpdatedBranches) != 1 {
		t.Fatalf("unexpected change: %+v", ch)
	}
	if err := g.SyncCheckout(ctx, mirror, checkout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "c")); err != nil {
		t.Fatal("checkout not updated")
	}

	// No-op fetch.
	before = after
	_ = g.FetchMirror(ctx, mirror, false)
	after, _ = g.SnapshotRefs(ctx, mirror)
	if ch := g.Diff(ctx, mirror, before, after); !ch.Empty() {
		t.Fatalf("expected no change, got %+v", ch)
	}

	// Deleted upstream branch is kept without prune, removed with prune.
	run(t, upstream, "branch", "-D", "feature")
	_ = g.FetchMirror(ctx, mirror, false)
	after, _ = g.SnapshotRefs(ctx, mirror)
	if _, ok := after["refs/heads/feature"]; !ok {
		t.Fatal("branch should be kept without prune")
	}
	_ = g.FetchMirror(ctx, mirror, true)
	after, _ = g.SnapshotRefs(ctx, mirror)
	if _, ok := after["refs/heads/feature"]; ok {
		t.Fatal("branch should be pruned")
	}
}

func TestRemoteSafetyBlocksLocal(t *testing.T) {
	tmp := t.TempDir()
	run(t, tmp, "init", "-q", "--bare", "up.git")
	g := New()
	if err := g.CloneMirror(context.Background(), filepath.Join(tmp, "up.git"), filepath.Join(tmp, "m.git")); err == nil {
		t.Fatal("expected local clone to be blocked")
	}
}
