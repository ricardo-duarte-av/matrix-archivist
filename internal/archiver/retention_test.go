package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

func TestPlanReleases(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	var cands []ReleaseCandidate
	for d := 1; d <= 6; d++ {
		cands = append(cands, ReleaseCandidate{Tag: fmt.Sprintf("v%d", d), Published: day(d), Size: 100})
	}
	tags := func(cs []ReleaseCandidate) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Tag)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		keep      int
		max, base int64
		want      string
	}{
		{0, 0, 0, "v6,v5,v4,v3,v2,v1"},
		{3, 0, 0, "v6,v5,v4"},
		{0, 350, 50, "v6,v5,v4"}, // 50 + 3×100 = 350 fits, a fourth doesn't
		{2, 1000, 50, "v6,v5"},   // count limit wins
		{0, 120, 50, "v6"},       // only the newest fits
		{0, 10, 500, "v6"},       // base already over the limit: still keep one
		{10, 2 << 30, 0, "v6,v5,v4,v3,v2,v1"},
	} {
		if got := tags(PlanReleases(cands, tc.keep, tc.max, tc.base)); got != tc.want {
			t.Errorf("keep=%d max=%d base=%d: got %s, want %s", tc.keep, tc.max, tc.base, got, tc.want)
		}
	}
	if got := tags(PlanReleases(nil, 10, 100, 0)); got != "" {
		t.Errorf("empty: %s", got)
	}
}

// End to end against a fake GitHub: limits stop old releases from being downloaded, and
// tightening them later removes archived ones, oldest first, never the last one.
func TestReleaseRetention(t *testing.T) {
	const assetSize = 1000
	type rel struct {
		Tag, Published string
	}
	var mu sync.Mutex
	rels := []rel{}
	for d := 1; d <= 5; d++ {
		rels = append(rels, rel{fmt.Sprintf("v%d", d), fmt.Sprintf("2026-01-0%dT00:00:00Z", d)})
	}
	downloads := map[string]int{}
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			downloads[r.URL.Path]++
			w.Write([]byte(strings.Repeat("x", assetSize)))
			return
		}
		var out []map[string]any
		for _, rl := range rels {
			out = append(out, map[string]any{"tag_name": rl.Tag, "published_at": rl.Published,
				"assets": []map[string]any{{"name": "a.bin", "size": assetSize, "browser_download_url": srvURL + "/dl/" + rl.Tag}}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	srvURL = srv.URL

	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfgFor := func(keep int, max string) *config.Config {
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
allow_internal_hosts: true # fake forge on 127.0.0.1
storage_dir: %s
releases: {keep_last: %d, max_repo_size: %q}
forges: [{host: git.invalid, type: github, api_url: %q}]
`, filepath.Join(tmp, "repos"), keep, max, srv.URL)))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	ctx := context.Background()
	repo, _ := st.AddRepo(ctx, "https://git.invalid/o/r", "git.invalid/o/r", "git.invalid/o/r", "@u:x")
	u, _ := gitops.ParseRepoURL(repo.URL)
	run := func(cfg *config.Config) (newTags, pruned []string) {
		a := New(cfg, st, zerolog.Nop())
		n, p, w := a.archiveReleases(ctx, repo, u, zerolog.Nop())
		if len(w) > 0 {
			t.Fatalf("warnings: %v", w)
		}
		return n, p
	}
	onDisk := func() string {
		es, _ := os.ReadDir(filepath.Join(tmp, "repos", repo.RelDir, "releases"))
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}

	// keep_last 3: only the newest three are ever downloaded.
	if n, _ := run(cfgFor(3, "")); fmt.Sprint(n) != "[v5 v4 v3]" || onDisk() != "v3,v4,v5" || len(downloads) != 3 {
		t.Fatalf("keep 3: new %v, disk %s, downloads %v", n, onDisk(), downloads)
	}
	// A new upstream release pushes the oldest kept one out.
	rels = append(rels, rel{"v6", "2026-01-06T00:00:00Z"})
	if n, p := run(cfgFor(3, "")); fmt.Sprint(n) != "[v6]" || fmt.Sprint(p) != "[v3]" || onDisk() != "v4,v5,v6" {
		t.Fatalf("new release: new %v pruned %v disk %s", n, p, onDisk())
	}
	// Size limit that fits about two releases (each is ~1 KB of asset plus notes).
	if _, p := run(cfgFor(3, "3000")); fmt.Sprint(p) != "[v4]" || onDisk() != "v5,v6" {
		t.Fatalf("size: pruned %v disk %s", p, onDisk())
	}
	// Even a tiny limit keeps the newest release.
	if _, p := run(cfgFor(3, "1")); fmt.Sprint(p) != "[v5]" || onDisk() != "v6" {
		t.Fatalf("tiny: pruned %v disk %s", p, onDisk())
	}
	if n, _ := st.ReleaseCount(ctx, repo.ID); n != 1 {
		t.Errorf("db has %d releases", n)
	}
	// Raising the limits brings releases back; nothing is re-downloaded needlessly.
	before := downloads["/dl/v6"]
	if n, p := run(cfgFor(0, "")); len(n) != 5 || len(p) != 0 || onDisk() != "v1,v2,v3,v4,v5,v6" || downloads["/dl/v6"] != before {
		t.Fatalf("unlimited: new %v pruned %v disk %s", n, p, onDisk())
	}
	// A release deleted upstream stays archived while it is within the limits.
	rels = rels[1:] // v1 gone upstream
	if _, p := run(cfgFor(10, "")); len(p) != 0 || !strings.HasPrefix(onDisk(), "v1,") {
		t.Fatalf("deleted upstream: pruned %v disk %s", p, onDisk())
	}
}
