package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

const testMatrix = `matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
`

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Light mode keeps the notes of every release but files only for the newest; a newer release
// moves the files along. Switching to full mode downloads the files the full limits allow and
// never deletes notes-only releases.
func TestLightReleases(t *testing.T) {
	type rel struct{ Tag, Published string }
	var mu sync.Mutex
	var rels []rel
	for d := 1; d <= 5; d++ {
		rels = append(rels, rel{fmt.Sprintf("v%d", d), fmt.Sprintf("2026-01-0%dT00:00:00Z", d)})
	}
	downloads := map[string]int{}
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			downloads[strings.TrimPrefix(r.URL.Path, "/dl/")]++
			w.Write([]byte(strings.Repeat("x", 1000)))
			return
		}
		var out []map[string]any
		for _, rl := range rels {
			out = append(out, map[string]any{"tag_name": rl.Tag, "published_at": rl.Published, "body": "notes of " + rl.Tag,
				"assets": []map[string]any{{"name": "a.bin", "size": 1000, "browser_download_url": srvURL + "/dl/" + rl.Tag}}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	srvURL = srv.URL

	tmp := t.TempDir()
	st := testStore(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(testMatrix+`
allow_internal_hosts: true # fake forge on 127.0.0.1
storage_dir: %s
releases: {keep_last: 3}
forges: [{host: git.invalid, type: github, api_url: %q}]
`, filepath.Join(tmp, "repos"), srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, _ := st.AddRepoMode(ctx, "https://git.invalid/o/r", "git.invalid/o/r", "git.invalid/o/r", "@u:x", store.ModeLight, "")
	u, _ := gitops.ParseRepoURL(repo.URL)
	a := New(cfg, st, zerolog.Nop())
	run := func() (newTags, pruned []string) {
		repo, _ = st.Repo(ctx, repo.ID)
		rr := a.archiveReleases(ctx, repo, u, zerolog.Nop())
		n, p, w := rr.New, rr.Pruned, rr.Warnings
		if len(w) > 0 {
			t.Fatalf("warnings: %v", w)
		}
		return n, p
	}
	relDir := filepath.Join(tmp, "repos", repo.RelDir, "releases")
	state := func() (notes, files string) {
		es, _ := os.ReadDir(relDir)
		var n, f []string
		for _, e := range es {
			if _, err := os.Stat(filepath.Join(relDir, e.Name(), "notes.md")); err == nil {
				n = append(n, e.Name())
			}
			if _, err := os.Stat(filepath.Join(relDir, e.Name(), "assets", "a.bin")); err == nil {
				f = append(f, e.Name())
			}
		}
		sort.Strings(n)
		sort.Strings(f)
		return strings.Join(n, ","), strings.Join(f, ",")
	}

	if n, _ := run(); len(n) != 5 {
		t.Fatalf("first run archived %v", n)
	}
	if notes, files := state(); notes != "v1,v2,v3,v4,v5" || files != "v5" || len(downloads) != 1 {
		t.Fatalf("light: notes %s, files %s, downloads %v", notes, files, downloads)
	}

	// A new release gets the files; the previous newest keeps its notes only.
	rels = append(rels, rel{"v6", "2026-01-06T00:00:00Z"})
	if n, p := run(); fmt.Sprint(n) != "[v6]" || fmt.Sprint(p) != "[v5]" {
		t.Fatalf("new release: new %v pruned %v", n, p)
	}
	if notes, files := state(); notes != "v1,v2,v3,v4,v5,v6" || files != "v6" {
		t.Fatalf("after v6: notes %s, files %s", notes, files)
	}
	if n, _ := run(); len(n) != 0 || downloads["v6"] != 1 {
		t.Fatalf("idle run: new %v downloads %v", n, downloads)
	}
	archived, _ := st.ArchivedReleases(ctx, repo.ID)
	metaOnly := 0
	for _, r := range archived {
		if r.MetaOnly {
			metaOnly++
		}
	}
	if len(archived) != 6 || metaOnly != 5 {
		t.Fatalf("db: %d releases, %d notes only", len(archived), metaOnly)
	}

	// Full mode (keep_last 3) adds the files of v4 and v5 and keeps every note.
	st.SetMode(ctx, repo.ID, store.ModeFull)
	if n, p := run(); len(n) != 0 || len(p) != 0 {
		t.Fatalf("to full: new %v pruned %v", n, p)
	}
	if notes, files := state(); notes != "v1,v2,v3,v4,v5,v6" || files != "v4,v5,v6" || downloads["v6"] != 1 {
		t.Fatalf("full: notes %s, files %s, downloads %v", notes, files, downloads)
	}

	// Back to light: files of all but the newest go, notes stay.
	st.SetMode(ctx, repo.ID, store.ModeLight)
	if _, p := run(); fmt.Sprint(p) != "[v4 v5]" {
		t.Fatalf("to light: pruned %v", p)
	}
	if notes, files := state(); notes != "v1,v2,v3,v4,v5,v6" || files != "v6" {
		t.Fatalf("light again: notes %s, files %s", notes, files)
	}
}

func TestDueRepos(t *testing.T) {
	st := testStore(t)
	cfg, err := config.Parse([]byte(testMatrix + `
check_interval: 1h
light: {check_interval: 12h, max_clones_per_cycle: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := New(cfg, st, zerolog.Nop())
	ctx := context.Background()
	add := func(name, mode string) *store.Repo {
		r, err := st.AddRepoMode(ctx, "https://git.invalid/o/"+name, "git.invalid/o/"+name, "git.invalid/o/"+name, "@u:x", mode, "")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	add("light-new1", store.ModeLight)
	full := add("full", store.ModeFull)
	fresh := add("light-fresh", store.ModeLight)
	stale := add("light-stale", store.ModeLight)
	add("light-new2", store.ModeLight)
	add("light-new3", store.ModeLight)
	for _, r := range []*store.Repo{full, fresh, stale} {
		st.MarkChecked(ctx, r.ID, true, nil)
	}
	repos, _ := st.Repos(ctx)
	byID := map[int64]string{}
	for _, r := range repos {
		byID[r.ID] = strings.TrimPrefix(r.URL, "https://git.invalid/o/")
	}
	names := func(ids []int64) string {
		var out []string
		for _, id := range ids {
			out = append(out, byID[id])
		}
		return strings.Join(out, ",")
	}
	now := time.Now()
	if got := names(a.DueRepos(repos, now, false)); got != "full,light-new1,light-new2" {
		t.Errorf("due now: %s", got)
	}
	// 11h later the light ones checked now are due (half a cycle early).
	if got := names(a.DueRepos(repos, now.Add(11*time.Hour+31*time.Minute), false)); got != "full,light-fresh,light-stale,light-new1,light-new2" {
		t.Errorf("due later: %s", got)
	}
	if got := names(a.DueRepos(repos, now, true)); got != "full,light-fresh,light-stale,light-new1,light-new2" {
		t.Errorf("forced: %s", got)
	}
}

func TestSourceSync(t *testing.T) {
	st := testStore(t)
	cfg, err := config.Parse([]byte(testMatrix + `
sources:
  - name: recomps
    url: http://recompbot:8080/api/recomps
    tags: [recomps]
    tag_fields: [kind, system]
    note_fields: [game, system]
    announce: >-
      Adding {{.repo}}, {{if eq .kind "toolkit"}}a toolkit for {{.system}}{{else}}a
      {{if eq .kind "reimpl"}}reimplementation{{else}}{{.kind}}{{end}} of **{{.game}}**
      from {{.system}}{{with .oses}}, for {{list .}}{{end}}{{end}}
`))
	if err != nil {
		t.Fatal(err)
	}
	src := cfg.Sources[0]
	if src.Mode != store.ModeLight || src.Interval != 15*time.Minute || src.URLField != "url" {
		t.Fatalf("source defaults: %+v", src)
	}
	a := New(cfg, st, zerolog.Nop())
	ctx := context.Background()
	manual, _ := st.AddRepo(ctx, "https://github.com/me/manual", "github.com/me/manual", "github.com/me/manual", "@u:x")

	body := func(urls ...string) []byte {
		var rows []map[string]any
		for _, u := range urls {
			rows = append(rows, map[string]any{"game": "Super Mario 64", "system": "N64", "kind": "recomp", "oses": []string{"Windows", "Linux"}, "url": u})
		}
		b, _ := json.Marshal(rows)
		return b
	}
	sync := func(data []byte) SyncResult {
		t.Helper()
		entries, err := ParseSource(data, src)
		if err != nil {
			t.Fatal(err)
		}
		res, err := a.applySource(ctx, src, entries)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := sync(body("https://github.com/a/one", "https://github.com/a/two/tree/main", "https://raw.githubusercontent.com/a/three/main/x.png",
		"https://github.com/me/manual", "https://github.com/a/one.git", "not a url"))
	if len(res.Added) != 3 || len(res.Invalid) != 1 || res.Listed != 4 {
		t.Fatalf("first sync: added %d invalid %v listed %d", len(res.Added), res.Invalid, res.Listed)
	}
	one, err := st.RepoByKey(ctx, "github.com/a/one")
	if err != nil || one.Mode != store.ModeLight || one.Source != "recomps" || one.Notes != "Super Mario 64 · N64" ||
		strings.Join(one.Tags, ",") != "n64,recomp,recomps" || one.AddedBy != "source:recomps" {
		t.Fatalf("added repo: %+v %v", one, err)
	}
	if three, err := st.RepoByKey(ctx, "github.com/a/three"); err != nil || three.URL != "https://github.com/a/three" {
		t.Fatalf("raw link: %+v %v", three, err)
	}
	if m, _ := st.Repo(ctx, manual.ID); m.Mode != store.ModeFull || m.Source != "" || len(m.Tags) != 0 {
		t.Fatalf("manual repo changed: %+v", m)
	}
	// Few additions: one notice each, rendered from the template.
	msgs := SyncMessages(src, res)
	if len(msgs) != 3 || msgs[0] != "📥 Adding https://github.com/a/one, a recomp of **Super Mario 64** from N64, for Windows and Linux (**#2**, light archive)" {
		t.Errorf("messages: %q", msgs)
	}
	// Many: one grouped message.
	many := res
	for range 4 {
		many.Added = append(many.Added, res.Added...)
		many.AddedEntries = append(many.AddedEntries, res.AddedEntries...)
	}
	if msgs := SyncMessages(src, many); len(msgs) != 1 || !strings.Contains(msgs[0], "15 new repositories queued") {
		t.Errorf("grouped: %q", msgs)
	}
	// A toolkit has no game.
	tk := SourceEntry{Fields: map[string]any{"kind": "toolkit", "system": "N64", "game": "", "oses": []any{}}}
	if got := Announce(src, one, tk); got != "📥 Adding https://github.com/a/one, a toolkit for N64 (**#2**, light archive)" {
		t.Errorf("toolkit: %q", got)
	}

	// Unchanged list: nothing to report.
	if res := sync(body("https://github.com/a/one", "https://github.com/a/two", "https://github.com/a/three")); !res.Empty() {
		t.Fatalf("idle sync: %+v", res)
	}

	// "two" disappears: tagged delisted, kept. A repo removed by hand isn't added back.
	threeRepo, _ := st.RepoByKey(ctx, "github.com/a/three")
	st.RemoveRepo(ctx, threeRepo.ID)
	st.Ignore(ctx, threeRepo.Key, "recomps")
	res = sync(body("https://github.com/a/one", "https://github.com/a/three"))
	if len(res.Delisted) != 1 || res.Delisted[0].Key != "github.com/a/two" || len(res.Added) != 0 {
		t.Fatalf("delist: %+v", res)
	}
	two, _ := st.RepoByKey(ctx, "github.com/a/two")
	if !slices.Contains(two.Tags, store.TagDelisted) {
		t.Fatalf("two tags: %v", two.Tags)
	}
	// An empty or much shorter list is not trusted for delisting.
	if res := sync([]byte("[]")); res.SkippedDelist != true || len(res.Delisted) != 0 {
		t.Fatalf("empty list: %+v", res)
	}
	// It comes back.
	res = sync(body("https://github.com/a/one", "https://github.com/a/two"))
	if len(res.Relisted) != 1 {
		t.Fatalf("relist: %+v", res)
	}
	if two, _ := st.RepoByKey(ctx, "github.com/a/two"); slices.Contains(two.Tags, store.TagDelisted) {
		t.Fatalf("still delisted: %v", two.Tags)
	}

	// Plain text lists work too.
	entries, err := ParseSource([]byte("# comment\nhttps://github.com/x/y\n\nhttps://gitlab.com/g/s/p  extra\n"), src)
	if err != nil || len(entries) != 2 || entries[1].URL != "https://gitlab.com/g/s/p" {
		t.Fatalf("text: %+v %v", entries, err)
	}
	if _, err := ParseSource([]byte(`{"error":"x"}`), src); err == nil {
		t.Error("JSON object accepted")
	}
}

func TestBuildSummaryLight(t *testing.T) {
	st := testStore(t)
	cfg, _ := config.Parse([]byte(testMatrix + "storage_dir: /tmp/x\n"))
	a := New(cfg, st, zerolog.Nop())
	ctx := context.Background()
	full, _ := st.AddRepo(ctx, "https://github.com/f/f", "github.com/f/f", "github.com/f/f", "@u:x")
	st.AddEvent(ctx, full.ID, store.EventRelease, "v1")
	for i := range 12 {
		k := fmt.Sprintf("github.com/l/l%d", i)
		r, _ := st.AddRepoMode(ctx, "https://"+k, k, k, "source:x", store.ModeLight, "x")
		st.AddEvent(ctx, r.ID, store.EventAdded, "initial clone")
		if i < 3 {
			st.AddEvent(ctx, r.ID, store.EventRelease, "v1")
		}
		if i == 11 {
			st.MarkChecked(ctx, r.ID, false, errString("boom"))
		}
	}
	text, empty, err := a.BuildSummary(ctx, time.Now().Add(-time.Hour))
	if err != nil || empty {
		t.Fatal(err, empty)
	}
	for _, want := range []string{"**Updated (1):**", "Light archives updated (12)**: 12 newly archived, 3 new releases", "Light archives failing (1)", "13 repositories tracked (12 light)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "github.com/l/l0 ") {
		t.Errorf("light repos listed one by one:\n%s", text)
	}
}

// A light archive has no checkout, so its README is read from the bare mirror.
func TestMirrorReadme(t *testing.T) {
	tmp := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	st := testStore(t)
	cfg, _ := config.Parse([]byte(testMatrix + "storage_dir: " + filepath.Join(tmp, "repos") + "\n"))
	a := New(cfg, st, zerolog.Nop())
	ctx := context.Background()
	repo, _ := st.AddRepoMode(ctx, "https://git.invalid/o/r", "git.invalid/o/r", "git.invalid/o/r", "@u:x", store.ModeLight, "")

	work := filepath.Join(tmp, "work")
	os.MkdirAll(work, 0o755)
	git(work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# Mario Recomp\n\nA native port of Super Mario 64.\n\nJoin us on https://discord.gg/abc\n"), 0o644)
	os.WriteFile(filepath.Join(work, "readme.txt"), []byte("other"), 0o644)
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "init")
	git(tmp, "clone", "-q", "--bare", work, a.mirrorDir(repo))

	info, ok := a.mirrorReadme(ctx, repo)
	if !ok || info.Title != "Mario Recomp" || info.Summary != "A native port of Super Mario 64." || len(info.Links) != 1 {
		t.Fatalf("readme: ok=%v %+v", ok, info)
	}
}
