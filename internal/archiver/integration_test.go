//go:build integration

// Run with: go test -tags 'goolm integration' ./internal/archiver -run Integration -v
// Requires network access.
package archiver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

func TestIntegrationRealRepos(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Parse([]byte(`
matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
storage_dir: ` + filepath.Join(tmp, "repos") + `
releases: {max_asset_size: 200KB}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, zerolog.New(zerolog.NewTestWriter(t)))

	urls := []string{
		"https://github.com/rs/xid",
		"https://codeberg.org/forgejo/docs",
	}
	if v := os.Getenv("ARCHIVIST_TEST_URLS"); v != "" {
		urls = []string{v}
	}
	for _, raw := range urls {
		u, err := gitops.ParseRepoURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		r, err := st.AddRepo(ctx, u.Raw, u.Key(), u.RelDir(), "@t:x")
		if err != nil {
			t.Fatal(err)
		}
		res := a.Check(ctx, r.ID)
		t.Logf("%s: %s", raw, res.Text())
		if res.Err != nil || !res.Cloned {
			t.Errorf("%s: clone failed: %v", raw, res.Err)
			continue
		}
		if _, err := os.Stat(filepath.Join(a.checkoutDir(r), ".git")); err != nil {
			t.Errorf("%s: no checkout", raw)
		}
		if m, _ := st.Repo(ctx, r.ID); m != nil {
			t.Logf("%s: description=%q homepage=%q topics=%v readme title=%q summary=%q links=%v",
				raw, m.Description, m.Homepage, m.Topics, m.ReadmeTitle, m.ReadmeSummary, m.Links)
			if m.ReadmeAt.IsZero() || m.ForgeInfoAt.IsZero() {
				t.Errorf("%s: metadata not collected", raw)
			}
		}
		res = a.Check(ctx, r.ID)
		t.Logf("%s (2nd): %s", raw, res.Text())
		if res.Err != nil || res.Changed() {
			t.Errorf("%s: second check should be a no-op: %s", raw, res.Text())
		}
	}
	text, _, err := a.BuildSummary(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + text)
}
