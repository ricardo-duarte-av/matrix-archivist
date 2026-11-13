package archiver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

func TestNextSummary(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Lisbon")
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, loc)
	if got := NextSummary(now, 9, 0, loc); !got.Equal(time.Date(2026, 11, 13, 9, 0, 0, 0, loc)) {
		t.Errorf("same day: %v", got)
	}
	now = time.Date(2026, 11, 13, 9, 0, 0, 0, loc)
	if got := NextSummary(now, 9, 0, loc); !got.Equal(time.Date(2026, 11, 14, 9, 0, 0, 0, loc)) {
		t.Errorf("next day: %v", got)
	}
}

func TestBuildSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Parse([]byte(`
matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
storage_dir: /tmp/x
`))
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, zerolog.Nop())

	r1, _ := st.AddRepo(ctx, "https://github.com/a/b", "github.com/a/b", "github.com/a/b", "@u:x")
	r2, _ := st.AddRepo(ctx, "https://github.com/c/d", "github.com/c/d", "github.com/c/d", "@u:x")
	if _, err := st.AddRepo(ctx, "https://github.com/a/b.git", "github.com/a/b", "github.com/a/b", "@u:x"); err != store.ErrDuplicate {
		t.Fatalf("expected duplicate, got %v", err)
	}
	for _, c := range []gitops.Change{{NewCommits: 3, UpdatedBranches: []string{"main"}}, {NewCommits: 2, UpdatedBranches: []string{"main"}, NewTags: []string{"v2"}}} {
		d, _ := json.Marshal(c)
		st.AddEvent(ctx, r1.ID, store.EventUpdate, string(d))
	}
	st.AddEvent(ctx, r1.ID, store.EventRelease, "v2")
	st.MarkChecked(ctx, r2.ID, false, errString("boom"))

	text, empty, err := a.BuildSummary(ctx, time.Now().Add(-time.Hour))
	if err != nil || empty {
		t.Fatalf("err=%v empty=%v", err, empty)
	}
	for _, want := range []string{"#1", "5 new commits", "1 new tag (v2)", "new release v2", "Failing (1)", "boom"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q:\n%s", want, text)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
