package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/mirror"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// fakeForge is a tiny Forgejo: the API calls the archiver makes, plus git over HTTP through
// git http-backend.
type fakeForge struct {
	t     *testing.T
	root  string // bare repositories, owner/name.git
	token string

	mu       sync.Mutex
	orgs     map[string]bool
	repos    map[string]*fakeRepo
	nextID   int64
	calls    []string
	gitAuth  []string
	failNext map[string]int // path prefix → status to answer once
}

type fakeRepo struct {
	Archived      bool
	Description   string
	Website       string
	DefaultBranch string
	Topics        []string
	Settings      map[string]any
	Releases      []*mirror.Release
	Files         map[int64][]byte // attachment id → content
}

func newFakeForge(t *testing.T) (*fakeForge, *httptest.Server) {
	f := &fakeForge{t: t, root: t.TempDir(), token: "secret", orgs: map[string]bool{}, repos: map[string]*fakeRepo{}, failNext: map[string]int{}}
	backend, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git not available")
	}
	git := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(backend)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + f.root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=bot"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			f.mu.Lock()
			f.gitAuth = append(f.gitAuth, r.Header.Get("Authorization"))
			f.mu.Unlock()
			git.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "token "+f.token {
			http.Error(w, `{"message":"unauthorized"}`, 401)
			return
		}
		f.api(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1"))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeForge) api(w http.ResponseWriter, r *http.Request, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+p)
	for prefix, status := range f.failNext {
		if strings.HasPrefix(r.Method+" "+p, prefix) {
			delete(f.failNext, prefix)
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"injected %d"}`, status)
			return
		}
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	body := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&body)
	}
	seg := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case p == "/user":
		reply(map[string]any{"login": "bot"})
	case r.Method == "GET" && len(seg) == 2 && seg[0] == "orgs":
		if !f.orgs[strings.ToLower(seg[1])] {
			http.NotFound(w, r)
			return
		}
		reply(map[string]any{})
	case r.Method == "POST" && p == "/orgs":
		f.orgs[strings.ToLower(body["username"].(string))] = true
		w.WriteHeader(201)
		reply(map[string]any{})
	case r.Method == "POST" && len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos":
		if !f.orgs[strings.ToLower(seg[1])] {
			http.NotFound(w, r)
			return
		}
		key := seg[1] + "/" + body["name"].(string)
		if err := exec.Command("git", "init", "-q", "--bare", filepath.Join(f.root, key+".git")).Run(); err != nil {
			f.t.Fatal(err)
		}
		f.repos[key] = &fakeRepo{Description: body["description"].(string), Files: map[int64][]byte{}}
		w.WriteHeader(201)
		reply(map[string]any{})
	case seg[0] == "repos" && len(seg) >= 3:
		repo := f.repos[seg[1]+"/"+seg[2]]
		if repo == nil {
			http.NotFound(w, r)
			return
		}
		f.repoAPI(w, r, repo, seg[1]+"/"+seg[2], seg[3:], body, reply)
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 400)
	}
}

func (f *fakeForge) repoAPI(w http.ResponseWriter, r *http.Request, repo *fakeRepo, key string, seg []string, body map[string]any, reply func(any)) {
	release := func(id string) *mirror.Release {
		n, _ := strconv.ParseInt(id, 10, 64)
		for _, rel := range repo.Releases {
			if rel.ID == n {
				return rel
			}
		}
		return nil
	}
	switch {
	case len(seg) == 0 && r.Method == "GET":
		reply(map[string]any{"archived": repo.Archived, "default_branch": repo.DefaultBranch})
	case len(seg) == 0 && r.Method == "DELETE":
		delete(f.repos, key)
		os.RemoveAll(filepath.Join(f.root, key+".git"))
		w.WriteHeader(204)
	case len(seg) == 0 && r.Method == "PATCH":
		if repo.Archived && body["archived"] != false {
			http.Error(w, `{"message":"repo is archived"}`, 422)
			return
		}
		if v, ok := body["archived"].(bool); ok {
			repo.Archived = v
		}
		if v, ok := body["description"].(string); ok {
			repo.Description = v
		}
		if v, ok := body["website"].(string); ok {
			repo.Website = v
		}
		if v, ok := body["default_branch"].(string); ok {
			repo.DefaultBranch = v
		}
		if _, ok := body["has_issues"]; ok {
			repo.Settings = body
		}
		reply(map[string]any{})
	case len(seg) == 1 && seg[0] == "topics":
		repo.Topics = nil
		for _, t := range body["topics"].([]any) {
			repo.Topics = append(repo.Topics, t.(string))
		}
		w.WriteHeader(204)
	case len(seg) == 1 && seg[0] == "releases" && r.Method == "GET":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		out := []*mirror.Release{}
		if page == 1 {
			out = repo.Releases
		}
		reply(out)
	case len(seg) == 1 && seg[0] == "releases" && r.Method == "POST":
		if repo.Archived {
			http.Error(w, `{"message":"archived"}`, 422)
			return
		}
		f.nextID++
		rel := &mirror.Release{ID: f.nextID, Tag: body["tag_name"].(string), Name: body["name"].(string), Body: body["body"].(string), Prerelease: body["prerelease"].(bool), Assets: []mirror.Attachment{}}
		repo.Releases = append(repo.Releases, rel)
		w.WriteHeader(201)
		reply(rel)
	case len(seg) == 2 && seg[0] == "releases" && r.Method == "PATCH":
		rel := release(seg[1])
		rel.Name, rel.Body, rel.Prerelease = body["name"].(string), body["body"].(string), body["prerelease"].(bool)
		reply(rel)
	case len(seg) == 2 && seg[0] == "releases" && r.Method == "DELETE":
		rel := release(seg[1])
		for i, x := range repo.Releases {
			if x == rel {
				repo.Releases = append(repo.Releases[:i], repo.Releases[i+1:]...)
				break
			}
		}
		w.WriteHeader(204)
	case len(seg) == 3 && seg[0] == "releases" && seg[2] == "assets" && r.Method == "POST":
		rel := release(seg[1])
		file, _, err := r.FormFile("attachment")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := io.ReadAll(file)
		f.nextID++
		repo.Files[f.nextID] = data
		rel.Assets = append(rel.Assets, mirror.Attachment{ID: f.nextID, Name: r.URL.Query().Get("name"), Size: int64(len(data))})
		w.WriteHeader(201)
		reply(map[string]any{})
	case len(seg) == 4 && seg[0] == "releases" && seg[2] == "assets" && r.Method == "DELETE":
		rel := release(seg[1])
		id, _ := strconv.ParseInt(seg[3], 10, 64)
		for i, a := range rel.Assets {
			if a.ID == id {
				rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
				break
			}
		}
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected "+r.Method+" "+strings.Join(seg, "/"), 400)
	}
}

func (f *fakeForge) releases(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.repos[key].Releases {
		var files []string
		for _, a := range r.Assets {
			files = append(files, fmt.Sprintf("%s:%d", a.Name, a.Size))
		}
		s := r.Tag + "[" + strings.Join(files, ",") + "]"
		if strings.Contains(r.Body, "not kept in the archive") {
			s += "(notes only)"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (f *fakeForge) countCalls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeRelease puts a release in the archive the way archiveReleases does.
func writeRelease(t *testing.T, a *Archiver, st *store.Store, repo *store.Repo, tag, published string, files map[string]string, metaOnly bool) {
	t.Helper()
	dir := a.releaseDir(repo, tag)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	pub, _ := time.Parse(time.RFC3339, published)
	notes := fmt.Sprintf("# Release %s\n\nTag: `%s`  \nPublished: %s\n\nChanges in %s.\n", tag, tag, published, tag)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte(notes), 0o644)
	os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"prerelease": false}`), 0o644)
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, "assets", name), []byte(content), 0o644)
	}
	if err := st.AddRelease(context.Background(), repo.ID, tag, "Release "+tag, pub, metaOnly); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorSync(t *testing.T) {
	forge, srv := newFakeForge(t)
	tmp := t.TempDir()
	st := testStore(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(testMatrix+`
storage_dir: %s
mirror: {enabled: true, url: %q, public_url: "https://git.example.org/", token: secret, max_new_per_cycle: 1}
`, filepath.Join(tmp, "repos"), srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := New(cfg, st, zerolog.New(zerolog.NewTestWriter(t)))
	if _, err := a.mirror.Login(ctx); err != nil {
		t.Fatal(err)
	}
	a.mirrorReady.Store(true)

	// The archive: a light repository with history, two tags and a preserved force push.
	repo, _ := st.AddRepoMode(ctx, "https://github.com/Zelda-Ret/oot", "github.com/zelda-ret/oot", "github.com/Zelda-Ret/oot", "@u:x", store.ModeLight, "recomps")
	st.SetNotes(ctx, repo.ID, "Ocarina of Time · N64")
	st.SetTags(ctx, repo.ID, []string{"recomp", "N64", "windows"})
	work := t.TempDir()
	gitT(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# oot\n"), 0o644)
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-qm", "one")
	gitT(t, work, "tag", "v1")
	old := gitT(t, work, "rev-parse", "HEAD")
	gitT(t, work, "commit", "-qm", "two", "--allow-empty")
	gitT(t, work, "tag", "v2")
	gitT(t, "", "clone", "-q", "--bare", work, a.mirrorDir(repo))
	gitT(t, a.mirrorDir(repo), "update-ref", "refs/archive/20260101T000000Z/heads/main", old)
	st.MarkChecked(ctx, repo.ID, true, nil)
	writeRelease(t, a, st, repo, "v1", "2026-01-01T00:00:00Z", nil, true)
	writeRelease(t, a, st, repo, "v2", "2026-02-01T00:00:00Z", map[string]string{"game.zip": "zipdata"}, false)
	writeRelease(t, a, st, repo, "gone-tag", "2026-03-01T00:00:00Z", nil, true) // its tag isn't in the archive

	// A second repository waits for the next pass (max_new_per_cycle: 1); an uncloned one is
	// never published.
	other, _ := st.AddRepo(ctx, "https://gitlab.com/zelda-ret/oot", "gitlab.com/zelda-ret/oot", "gitlab.com/zelda-ret/oot", "@u:x")
	gitT(t, "", "clone", "-q", "--bare", work, a.mirrorDir(other))
	st.MarkChecked(ctx, other.ID, true, nil)
	st.AddRepo(ctx, "https://github.com/x/uncloned", "github.com/x/uncloned", "github.com/x/uncloned", "@u:x")

	pass := a.MirrorPass(ctx)
	if pass.Created != 1 || pass.Queued != 1 || pass.Failed != 0 {
		t.Fatalf("first pass: %+v", pass)
	}
	key := "Zelda-Ret/oot"
	fr := forge.repos[key]
	if fr == nil {
		t.Fatalf("forge repo not created: %v", forge.calls)
	}
	bare := filepath.Join(forge.root, key+".git")
	refs := gitT(t, bare, "for-each-ref", "--format=%(refname)")
	for _, want := range []string{"refs/heads/main", "refs/tags/v1", "refs/tags/v2", "refs/heads/archived/20260101T000000Z/main"} {
		if !strings.Contains(refs, want) {
			t.Errorf("forge refs missing %s:\n%s", want, refs)
		}
	}
	for _, auth := range forge.gitAuth {
		if auth != a.mirror.PushAuth() {
			t.Fatalf("git request without the token: %q", auth)
		}
	}
	if fr.DefaultBranch != "main" || fr.Website != "https://github.com/Zelda-Ret/oot" ||
		fr.Description != "Ocarina of Time · N64" || strings.Join(fr.Topics, ",") != "n64,recomp,windows" {
		t.Errorf("forge meta: %+v", fr)
	}
	if fr.Settings["has_issues"] != false || fr.Settings["has_actions"] != false || fr.Settings["has_pull_requests"] != false {
		t.Errorf("forge settings: %v", fr.Settings)
	}
	if got := forge.releases(key); got != "v1[](notes only) v2[game.zip:7]" {
		t.Errorf("forge releases: %s", got)
	}
	if b := forge.repos[key].Releases[0].Body; !strings.HasPrefix(b, "Changes in v1.") || !strings.Contains(b, "originally published 2026-01-01") {
		t.Errorf("release body: %q", b)
	}

	// Nothing changed: the next pass publishes the queued repository (same owner/name, so it
	// gets the host appended) and makes no API call for the first one.
	before := forge.countCalls("")
	pass = a.MirrorPass(ctx)
	if pass.Created != 1 || pass.Updated != 0 || pass.Queued != 0 {
		t.Fatalf("second pass: %+v", pass)
	}
	if forge.repos["zelda-ret/oot-gitlab.com"] == nil {
		t.Fatalf("second repo: %v", forge.calls[before:])
	}
	before = forge.countCalls("")
	if pass = a.MirrorPass(ctx); pass.Created+pass.Updated+pass.Failed != 0 || forge.countCalls("") != before {
		t.Fatalf("idle pass: %+v, calls %v", pass, forge.calls[before:])
	}

	// A new release: v2 loses its files (light mode moved them to v3), v1 was deleted from
	// the archive. New commits are pushed.
	gitT(t, work, "commit", "-qm", "three", "--allow-empty")
	gitT(t, work, "tag", "v3")
	gitT(t, a.mirrorDir(repo), "fetch", "-q", work, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	writeRelease(t, a, st, repo, "v3", "2026-04-01T00:00:00Z", map[string]string{"game.zip": "zipdata3"}, false)
	os.RemoveAll(filepath.Join(a.releaseDir(repo, "v2"), "assets"))
	st.SetReleaseMetaOnly(ctx, repo.ID, "v2")
	st.RemoveRelease(ctx, repo.ID, "v1")
	res := a.SyncMirror(ctx, repo.ID, false)
	if res.Err != nil || !res.Pushed || res.Created {
		t.Fatalf("update: %+v", res)
	}
	if got := forge.releases(key); got != "v2[](notes only) v3[game.zip:8]" {
		t.Errorf("forge releases after update: %s", got)
	}
	if head := gitT(t, bare, "rev-parse", "refs/tags/v3"); head != gitT(t, work, "rev-parse", "v3") {
		t.Errorf("v3 not pushed")
	}

	// A failure is recorded and retried after the back-off; the forge repository is checked
	// again on the retry.
	gitT(t, work, "commit", "-qm", "four", "--allow-empty")
	gitT(t, a.mirrorDir(repo), "fetch", "-q", work, "+refs/heads/*:refs/heads/*")
	forge.mu.Lock()
	forge.failNext["PATCH /repos/Zelda-Ret/oot"] = 500
	forge.mu.Unlock()
	st.SetTags(ctx, repo.ID, []string{"recomp"})
	if res = a.SyncMirror(ctx, repo.ID, false); res.Err == nil {
		t.Fatal("expected an injected failure")
	}
	ms, _, _ := st.MirrorState(ctx, repo.ID)
	if ms.FailCount != 1 || ms.MetaHash != "" || !strings.Contains(ms.Error, "injected 500") {
		t.Fatalf("failure state: %+v", ms)
	}
	states, _ := st.MirrorStates(ctx)
	repos, _ := st.Repos(ctx)
	if ids, _ := a.MirrorDue(repos, states, time.Now()); len(ids) != 1 || ids[0] != other.ID {
		t.Fatalf("failed repo should wait for its back-off: %v", ids)
	}
	if ids, _ := a.MirrorDue(repos, states, time.Now().Add(2*time.Hour)); len(ids) != 2 {
		t.Fatalf("failed repo should be retried after its back-off: %v", ids)
	}
	if res = a.SyncMirror(ctx, repo.ID, false); res.Err != nil {
		t.Fatalf("retry: %v", res.Err)
	}
	if strings.Join(forge.repos[key].Topics, ",") != "recomp" {
		t.Errorf("topics after retry: %v", forge.repos[key].Topics)
	}

	// Removing the repository archives its forge copy; adding it back un-archives it.
	if err := a.Remove(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	if !forge.repos[key].Archived {
		t.Fatal("removed repository not archived on the forge")
	}
	again, _ := st.AddRepoMode(ctx, repo.URL, repo.Key, repo.RelDir, "@u:x", store.ModeLight, "")
	st.MarkChecked(ctx, again.ID, true, nil)
	if res = a.SyncMirror(ctx, again.ID, false); res.Err != nil || res.Path != key {
		t.Fatalf("re-added: %+v", res)
	}
	if forge.repos[key].Archived {
		t.Error("re-added repository still archived on the forge")
	}
	// Purging deletes it.
	if err := a.Remove(ctx, again, true); err != nil {
		t.Fatal(err)
	}
	if forge.repos[key] != nil {
		t.Error("purged repository still on the forge")
	}
}

func TestMirrorNames(t *testing.T) {
	for _, c := range []struct{ host, path, want string }{
		{"github.com", "zeldaret/oot", "zeldaret/oot"},
		{"gitlab.com", "group/sub/project", "group/sub-project"},
		{"github.com", "api/thing", "api-mirror/thing"},
		{"github.com", "-weird--/x.git", "weird/x-git"},
		{"codeberg.org", "only", "codeberg/only"},
	} {
		o, n := mirror.Path(c.host, c.path)
		if o+"/"+n != c.want {
			t.Errorf("Path(%s, %s) = %s/%s, want %s", c.host, c.path, o, n, c.want)
		}
	}
	if got := strings.Join(mirror.Topics([]string{"N64", "Windows 10", "-x", "n64", strings.Repeat("a", 40)}), ","); got != "n64,windows-10,x,"+strings.Repeat("a", 35) {
		t.Errorf("topics: %s", got)
	}
	if got := mirror.WithSuffix("oot", "gitlab.com"); got != "oot-gitlab.com" {
		t.Errorf("suffix: %s", got)
	}
	title, body := parseNotes("# My release\n\nTag: `v1`  \nPublished: 2026-01-01T00:00:00Z\n\nLine one\n\nLine two\n")
	if title != "My release" || body != "Line one\n\nLine two" {
		t.Errorf("parseNotes: %q %q", title, body)
	}
}
