package archiver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// A repository that was archived and then gets a DMCA takedown: one @room alert naming the
// takedown, no extra "failed N times" alert, and it shows up as preserved in the summary.
func TestTakedownAlert(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		fmt.Fprint(w, `{"message":"Repository access blocked","block":{"reason":"dmca","html_url":"https://example.org/notice"}}`)
	}))
	defer api.Close()

	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
storage_dir: %s
git_timeout: 20s
failure_alert_threshold: 3
releases: {enabled: false}
forges: [{host: git.invalid, type: github, api_url: %q}]
http: {public_url: "https://archive.example.org"}
`, filepath.Join(tmp, "repos"), api.URL)))
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, zerolog.Nop())
	var mu sync.Mutex
	var alerts, notices []string
	a.Alert = func(_ context.Context, s string) { mu.Lock(); alerts = append(alerts, s); mu.Unlock() }
	a.Notify = func(_ context.Context, s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }

	repo, _ := st.AddRepo(ctx, "https://git.invalid/o/r", "git.invalid/o/r", "git.invalid/o/r", "@u:x")
	// Pretend it was archived before the takedown.
	st.MarkChecked(ctx, repo.ID, true, nil)
	st.SetSize(ctx, repo.ID, 1<<20)

	for range 5 {
		if res := a.Check(ctx, repo.ID); res.Err == nil {
			t.Fatal("expected the fetch to fail")
		}
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "taken down upstream") || !strings.Contains(alerts[0], "DMCA takedown") ||
		!strings.Contains(alerts[0], "archive.example.org/browse/git.invalid/o/r/") {
		t.Fatalf("alerts: %q", alerts)
	}
	r, _ := st.Repo(ctx, repo.ID)
	if r.Upstream != store.UpstreamLegal || r.UpstreamSince.IsZero() {
		t.Fatalf("repo upstream: %+v", r)
	}
	text, _, _ := a.BuildSummary(ctx, r.AddedAt.Add(-1))
	if !strings.Contains(text, "Gone upstream, preserved here (1)") {
		t.Errorf("summary:\n%s", text)
	}

	// A never-archived repo with a bad URL is a plain failure, not a takedown.
	bad, _ := st.AddRepo(ctx, "https://git.invalid/o/typo", "git.invalid/o/typo", "git.invalid/o/typo", "@u:x")
	for range 3 {
		a.Check(ctx, bad.ID)
	}
	if len(alerts) != 2 || !strings.Contains(alerts[1], "failed 3 times in a row") {
		t.Fatalf("alerts: %q", alerts)
	}
	if r, _ := st.Repo(ctx, bad.ID); r.Upstream != "" {
		t.Errorf("never-cloned repo flagged upstream: %+v", r)
	}

	var sb strings.Builder
	if err := a.WriteMetrics(ctx, &sb); err != nil {
		t.Fatal(err)
	}
	m := sb.String()
	for _, want := range []string{
		"archivist_repos 2\n",
		"archivist_repos_gone_upstream 1\n",
		`archivist_repo_gone_upstream{id="1",repo="git.invalid/o/r"} 1`,
		`archivist_repo_size_bytes{id="1",repo="git.invalid/o/r"} 1048576`,
		`archivist_checks_total{result="error"} 8`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q:\n%s", want, m)
		}
	}
}

func TestRewriteNotice(t *testing.T) {
	r := &store.Repo{ID: 4, URL: "https://x.org/a/b"}
	if s := RewriteNoticeText(r, gitops.Change{RewrittenBranches: []string{"feature"}}, "main"); s != "" {
		t.Errorf("feature-branch rewrite should only go to the summary: %q", s)
	}
	s := RewriteNoticeText(r, gitops.Change{RewrittenBranches: []string{"main"}, MovedTags: []string{"v1"}, Snapshot: "refs/archive/X"}, "main")
	for _, want := range []string{"#4", "history of `main` was rewritten", "tags were moved: v1", "refs/archive/X/"} {
		if !strings.Contains(s, want) {
			t.Errorf("notice missing %q: %s", want, s)
		}
	}
}
