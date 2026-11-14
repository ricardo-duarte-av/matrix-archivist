package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The live database of an existing install is at schema version 1; it must upgrade in place.
func TestMigrateFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (1)`,
		migrations[0],
		`INSERT INTO repos (url, key, rel_dir, added_by, added_at, cloned) VALUES ('https://x.org/a/b', 'x.org/a/b', 'x.org/a/b', '@u:x', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Repo(ctx, 1)
	if err != nil || r.URL != "https://x.org/a/b" || !r.Cloned || r.Upstream != "" || len(r.Tags) != 0 {
		t.Fatalf("repo after upgrade: %+v %v", r, err)
	}

	if err := s.SetTags(ctx, 1, []string{"Video", " cli ", "video", "#a,b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpstream(ctx, 1, UpstreamGone, "404"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Repo(ctx, 1)
	since := r.UpstreamSince
	if len(r.Tags) != 3 || r.Tags[0] != "ab" || r.Tags[1] != "cli" || r.Tags[2] != "video" || r.Upstream != UpstreamGone || since.IsZero() {
		t.Fatalf("after set: %+v", r)
	}
	// Same status again keeps the original time; clearing resets it.
	s.SetUpstream(ctx, 1, UpstreamGone, "404 again")
	if r, _ = s.Repo(ctx, 1); !r.UpstreamSince.Equal(since) || r.UpstreamDetail != "404 again" {
		t.Fatalf("since changed: %+v", r)
	}
	s.SetUpstream(ctx, 1, "", "")
	if r, _ = s.Repo(ctx, 1); r.Upstream != "" || !r.UpstreamSince.IsZero() {
		t.Fatalf("after clear: %+v", r)
	}
}
