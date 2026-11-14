package store

import (
	"context"
	"fmt"
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

func TestSearchRepos(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.AddRepo(ctx, "https://github.com/x/TheItalianJob", "github.com/x/theitalianjob", "a", "@u:x")
	b, _ := s.AddRepo(ctx, "https://github.com/y/zelda3", "github.com/y/zelda3", "b", "@u:x")
	c, _ := s.AddRepo(ctx, "https://github.com/z/other_100%", "github.com/z/other", "c", "@u:x")
	s.SetReadme(ctx, a.ID, Readme{Title: "Mario", Summary: "Static recompilation", Text: "# Mario\nA Super Mario World recompilation for PC.", Links: []string{"discord.gg/abc"}})
	s.SetForgeInfo(ctx, b.ID, "A reimplementation of Zelda 3", "https://zelda.example", []string{"snes", "zelda"}, true)
	s.SetTags(ctx, b.ID, []string{"nintendo"})
	s.SetTags(ctx, c.ID, []string{"nintendo-ds"})

	ids := func(terms ...string) (out []int64) {
		rs, err := s.SearchRepos(ctx, terms)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return
	}
	check := func(got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	check(ids("super mario"), a.ID)    // README body
	check(ids("MARIO", "world"), a.ID) // case-insensitive, AND
	check(ids("mario", "zelda"))       // no repo has both
	check(ids("discord.gg/abc"), a.ID) // links
	check(ids("snes"), b.ID)           // topics
	check(ids("#nintendo"), b.ID)      // exact tag, not nintendo-ds
	check(ids("nintendo"), b.ID, c.ID) // substring of tags
	check(ids("100%"), c.ID)           // % is literal
	check(ids("y_z"))                  // _ is literal
	check(ids("other_"), c.ID)

	r, _ := s.Repo(ctx, b.ID)
	if r.Description != "A reimplementation of Zelda 3" || r.Homepage != "https://zelda.example" || fmt.Sprint(r.Topics) != "[snes zelda]" || r.ForgeInfoAt.IsZero() {
		t.Errorf("forge info not loaded: %+v", r)
	}
	r, _ = s.Repo(ctx, a.ID)
	if r.ReadmeSummary != "Static recompilation" || fmt.Sprint(r.Links) != "[discord.gg/abc]" || r.ReadmeAt.IsZero() {
		t.Errorf("readme not loaded: %+v", r)
	}
	// A failed forge lookup keeps the old data and records the attempt.
	s.SetForgeInfo(ctx, b.ID, "", "", nil, false)
	if r, _ := s.Repo(ctx, b.ID); r.Description == "" {
		t.Error("failed lookup erased description")
	}
}
