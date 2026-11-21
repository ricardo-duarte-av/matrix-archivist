package archiver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/live"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/mirror"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// MirrorEnabled reports whether repositories are published to a mirror forge.
func (a *Archiver) MirrorEnabled() bool { return a.mirror != nil }

// KickMirror asks the mirror loop to run a pass soon (no-op when the mirror is disabled).
func (a *Archiver) KickMirror() {
	if a.mirror == nil {
		return
	}
	select {
	case a.mirrorKick <- struct{}{}:
	default:
	}
}

// runMirror publishes repositories to the forge after every check cycle (and when kicked).
func (a *Archiver) runMirror(ctx context.Context) {
	log := a.log.With().Str("forge", a.cfg.Mirror.URL).Logger()
	for wait := time.Minute; ; wait = min(wait*2, time.Hour) {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		login, err := a.mirror.Login(lctx)
		cancel()
		if err == nil {
			log.Info().Str("user", login).Msg("Connected to the mirror forge")
			break
		}
		log.Warn().Err(err).Dur("retry_in", wait).Msg("Can't reach the mirror forge")
		a.Live.Event(0, "", "error", "Can't reach the mirror forge: "+oneLine(err.Error()))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
	a.mirrorReady.Store(true)
	ticker := time.NewTicker(a.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.mirrorKick:
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
		a.MirrorPass(ctx)
	}
}

// MirrorResult is what one repository's sync to the forge did.
type MirrorResult struct {
	Repo      *store.Repo
	Path      string // owner/name on the forge
	Created   bool   // the forge repository was created in this sync
	Pushed    bool
	Releases  int // releases created, edited or deleted on the forge
	Files     int // files uploaded or deleted
	Unchanged bool
	Busy      bool // the repository was being checked; try again later
	Warnings  []string
	Err       error
}

// Text is a one-line description of the result.
func (r MirrorResult) Text() string {
	switch {
	case r.Err != nil:
		return "❌ " + oneLine(r.Err.Error())
	case r.Busy:
		return "busy with a check, will sync later"
	case r.Unchanged:
		return "already in sync"
	}
	var parts []string
	if r.Created {
		parts = append(parts, "published")
	} else if r.Pushed {
		parts = append(parts, "git pushed")
	}
	if r.Releases > 0 {
		parts = append(parts, fmt.Sprintf("%d %s synced", r.Releases, plural(r.Releases, "release", "releases")))
	}
	if r.Files > 0 {
		parts = append(parts, fmt.Sprintf("%d %s synced", r.Files, plural(r.Files, "file", "files")))
	}
	if len(parts) == 0 {
		parts = append(parts, "settings updated")
	}
	s := strings.Join(parts, ", ")
	if len(r.Warnings) > 0 {
		s += " (⚠️ " + strings.Join(r.Warnings, "; ") + ")"
	}
	return s
}

// MirrorPassResult counts what a pass over every repository did.
type MirrorPassResult struct {
	Ran                              bool
	Created, Updated, Failed, Queued int
}

// mirrorBackoff is how long to wait before retrying a repository that failed n times in a row.
func mirrorBackoff(n int) time.Duration {
	return time.Duration(min(n*n, 24)) * time.Hour
}

// MirrorDue picks the repositories a pass should sync: archived ones already published (to
// push what changed), then those never published, at most mirror.max_new_per_cycle of them.
// Repositories that failed recently are left out until their back-off has passed. queued is
// the number of new ones left for later passes.
func (a *Archiver) MirrorDue(repos []*store.Repo, states map[int64]store.MirrorState, now time.Time) (ids []int64, queued int) {
	var news []int64
	for _, r := range repos {
		if !r.Cloned {
			continue
		}
		st, ok := states[r.ID]
		if ok && st.FailCount > 0 && now.Sub(st.TriedAt) < mirrorBackoff(st.FailCount) {
			continue
		}
		if ok && !st.SyncedAt.IsZero() {
			ids = append(ids, r.ID)
		} else {
			news = append(news, r.ID)
		}
	}
	if max := a.cfg.Mirror.MaxNewPerCycle; max > 0 && len(news) > max {
		queued = len(news) - max
		news = news[:max]
	}
	return append(ids, news...), queued
}

// MirrorPass syncs every repository that is due to the forge (see MirrorDue).
func (a *Archiver) MirrorPass(ctx context.Context) (out MirrorPassResult) {
	if a.mirror == nil || !a.mirrorReady.Load() || !a.mirrorRunning.CompareAndSwap(false, true) {
		return out
	}
	defer a.mirrorRunning.Store(false)
	out.Ran = true
	repos, err := a.store.Repos(ctx)
	if err != nil {
		a.log.Err(err).Msg("Failed to list repos")
		return out
	}
	states, err := a.store.MirrorStates(ctx)
	if err != nil {
		a.log.Err(err).Msg("Failed to load mirror states")
		return out
	}
	start := time.Now()
	ids, queued := a.MirrorDue(repos, states, start)
	out.Queued = queued

	jobs := make(chan int64)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range a.cfg.Mirror.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				res := a.syncMirror(ctx, id, false)
				mu.Lock()
				switch {
				case res.Err != nil:
					out.Failed++
				case res.Created:
					out.Created++
				case !res.Unchanged && !res.Busy:
					out.Updated++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if out.Created+out.Updated+out.Failed > 0 || queued > 0 {
		text := fmt.Sprintf("Forge sync finished in %s: %d published, %d updated, %d failed",
			time.Since(start).Round(time.Second), out.Created, out.Updated, out.Failed)
		if queued > 0 {
			text += fmt.Sprintf(", %d waiting for the next pass", queued)
		}
		a.Live.Event(0, "", "mirror", text)
		a.log.Info().Int("published", out.Created).Int("updated", out.Updated).Int("failed", out.Failed).
			Int("queued", queued).Dur("took", time.Since(start)).Msg("Forge sync finished")
	}
	return out
}

// SyncMirror publishes one repository to the forge now, waiting for a running check to end.
// With force, everything is published again even if nothing seems to have changed.
func (a *Archiver) SyncMirror(ctx context.Context, id int64, force bool) MirrorResult {
	if a.mirror == nil {
		return MirrorResult{Err: errors.New("the forge mirror is not enabled")}
	}
	if !a.mirrorReady.Load() {
		if _, err := a.mirror.Login(ctx); err != nil {
			return MirrorResult{Err: err}
		}
		a.mirrorReady.Store(true)
	}
	if force {
		if err := a.store.ResetMirror(ctx, id); err != nil {
			return MirrorResult{Err: err}
		}
	}
	return a.syncMirror(ctx, id, true)
}

func (a *Archiver) syncMirror(ctx context.Context, id int64, wait bool) (res MirrorResult) {
	m := a.lock(id)
	if wait {
		m.Lock()
	} else if !m.TryLock() {
		return MirrorResult{Busy: true}
	}
	defer m.Unlock()

	repo, err := a.store.Repo(ctx, id)
	if err != nil {
		return MirrorResult{Err: err}
	}
	res.Repo = repo
	if !repo.Cloned || !dirExists(a.mirrorDir(repo)) {
		res.Err = errors.New("not archived yet")
		return res
	}
	st, _, err := a.store.MirrorState(ctx, id)
	if err != nil {
		res.Err = err
		return res
	}
	if st.Path == "" {
		if st.Path, err = a.mirrorPath(ctx, repo); err != nil {
			res.Err = err
			return res
		}
	}
	res.Path = st.Path
	owner, name, _ := strings.Cut(st.Path, "/")
	log := a.log.With().Int64("repo_id", repo.ID).Str("forge_repo", st.Path).Logger()

	dir := a.mirrorDir(repo)
	refsHash, err := a.git.RefsFingerprint(ctx, dir)
	if err != nil {
		res.Err = err
		return res
	}
	meta := a.mirrorMeta(ctx, repo, owner, name)
	metaHash := hashJSON(meta)
	var rels []localRelease
	relHash := ""
	if a.cfg.Mirror.Releases {
		rels = a.localReleases(ctx, repo)
		relHash = hashJSON(struct {
			Refs  string
			Files bool
			Rels  []localRelease
		}{refsHash, a.cfg.Mirror.ReleaseFiles, rels})
	}
	if refsHash == st.RefsHash && metaHash == st.MetaHash && relHash == st.ReleasesHash {
		res.Unchanged = true
		return res
	}

	slot := live.From(ctx)
	if slot == nil {
		slot = a.Live.Temp("Forge sync")
		defer slot.End()
		ctx = live.WithSlot(ctx, slot)
	}
	slot.Begin(repo.ID, repo.URL, "→ "+a.cfg.Mirror.RepoURL(st.Path))

	st.RepoID = repo.ID
	st.TriedAt = time.Now()
	fail := func(err error) MirrorResult {
		res.Err = err
		st.Error, st.FailCount = oneLine(err.Error()), st.FailCount+1
		// Re-check the forge repository itself on the next attempt (it may have been deleted).
		st.MetaHash = ""
		_ = a.store.SaveMirror(ctx, st)
		log.Warn().Err(err).Int("failures", st.FailCount).Msg("Forge sync failed")
		a.Live.Event(repo.ID, repo.URL, "error", "Forge sync failed: "+oneLine(err.Error()))
		return res
	}

	if st.MetaHash != metaHash || st.SyncedAt.IsZero() {
		slot.SetStep("Preparing forge repository", "")
		created, err := a.ensureForgeRepo(ctx, owner, name, meta)
		if err != nil {
			return fail(err)
		}
		res.Created = created
	}
	if refsHash != st.RefsHash {
		slot.SetStep("Pushing to forge", "")
		gctx, cancel := context.WithTimeout(ctx, a.cfg.GitTimeout)
		err := a.git.PushMirror(gctx, dir, a.mirror.GitURL(owner, name), a.mirror.PushAuth(), a.cfg.Mirror.Snapshots)
		cancel()
		if err != nil {
			return fail(err)
		}
		st.RefsHash, res.Pushed = refsHash, true
	}
	if st.MetaHash != metaHash {
		slot.SetStep("Updating forge settings", "")
		if err := a.applyForgeMeta(ctx, owner, name, meta); err != nil {
			return fail(err)
		}
		st.MetaHash = metaHash
	}
	if relHash != st.ReleasesHash {
		refs, err := a.git.SnapshotRefs(ctx, dir)
		if err != nil {
			return fail(err)
		}
		ready, err := a.waitForgeNotEmpty(ctx, owner, name, refs)
		if err != nil {
			return fail(err)
		}
		if ready {
			n, files, warnings, err := a.syncForgeReleases(ctx, repo, owner, name, rels, refs)
			res.Releases, res.Files, res.Warnings = n, files, warnings
			if err != nil {
				return fail(err)
			}
		} else if len(rels) > 0 {
			res.Warnings = append(res.Warnings, "releases not published: the archive has no branches, and the forge takes no releases in an empty repository")
		}
		st.ReleasesHash = relHash
	}
	st.SyncedAt, st.FailCount, st.Error = time.Now(), 0, ""
	if len(res.Warnings) > 0 {
		st.Error = "warning: " + oneLine(strings.Join(res.Warnings, "; "))
	}
	if err := a.store.SaveMirror(ctx, st); err != nil {
		log.Err(err).Msg("Failed to save mirror state")
	}
	if res.Created {
		log.Info().Msg("Published to the forge")
		a.Live.Event(repo.ID, repo.URL, "mirror", "Published to the forge: "+a.cfg.Mirror.RepoURL(st.Path))
	} else {
		log.Debug().Str("result", res.Text()).Msg("Synced to the forge")
	}
	for _, w := range res.Warnings {
		log.Warn().Str("warning", w).Msg("Forge sync warning")
	}
	return res
}

// mirrorPath picks the forge owner/name for a repository: the upstream owner and name, with
// the upstream host (then the repository id) appended to the name if another repository has
// that path already.
func (a *Archiver) mirrorPath(ctx context.Context, repo *store.Repo) (string, error) {
	host, path, _ := strings.Cut(repo.RelDir, "/")
	if u, err := gitops.ParseRepoURL(repo.URL); err == nil {
		host, path = u.Host, u.Path
	}
	owner, name := mirror.Path(host, path)
	for _, cand := range []string{name, mirror.WithSuffix(name, host), mirror.WithSuffix(name, strconv.FormatInt(repo.ID, 10))} {
		taken, err := a.store.MirrorPathTaken(ctx, owner+"/"+cand, repo.ID)
		if err != nil {
			return "", err
		}
		if !taken {
			return owner + "/" + cand, nil
		}
	}
	return "", fmt.Errorf("no free forge name for %s/%s", owner, name)
}

// forgeMeta is what the forge repository's page shows besides git and releases.
type forgeMeta struct {
	Owner, Name   string
	OrgURL        string
	Description   string
	Website       string
	DefaultBranch string
	Topics        []string
	Private       bool
}

func (a *Archiver) mirrorMeta(ctx context.Context, repo *store.Repo, owner, name string) forgeMeta {
	m := forgeMeta{Owner: owner, Name: name, Private: a.cfg.Mirror.Private}
	host, path, _ := strings.Cut(repo.RelDir, "/")
	if u, err := gitops.ParseRepoURL(repo.URL); err == nil {
		host, path = u.Host, u.Path
	}
	m.Website = "https://" + host + "/" + path
	if i := strings.LastIndex(path, "/"); i > 0 {
		m.OrgURL = "https://" + host + "/" + path[:i]
	}
	var desc []string
	if n := strings.Join(strings.Fields(repo.Notes), " "); n != "" {
		desc = append(desc, n)
	}
	if about := strings.Join(strings.Fields(repo.About()), " "); about != "" {
		desc = append(desc, about)
	}
	m.Description = strings.Join(desc, " — ")
	if m.Description == "" {
		m.Description = "Archive of " + m.Website
	}
	m.Description = truncateRunes(m.Description, 255)
	topics := append(append([]string{}, repo.Tags...), repo.Topics...)
	switch repo.Upstream {
	case store.UpstreamGone:
		topics = append([]string{"gone-upstream"}, topics...)
	case store.UpstreamLegal:
		topics = append([]string{"taken-down"}, topics...)
	case store.UpstreamBlocked:
		topics = append([]string{"blocked-upstream"}, topics...)
	}
	m.Topics = mirror.Topics(topics)
	// Only a branch that exists can be the default (HEAD of an empty clone points nowhere).
	if b := a.git.DefaultBranch(ctx, a.mirrorDir(repo)); b != "" {
		if refs, err := a.git.SnapshotRefs(ctx, a.mirrorDir(repo)); err == nil && refs["refs/heads/"+b] != "" {
			m.DefaultBranch = b
		}
	}
	return m
}

// ensureForgeRepo makes sure the organization and the repository exist and that the
// repository isn't archived (read-only) on the forge, which would refuse the push.
func (a *Archiver) ensureForgeRepo(ctx context.Context, owner, name string, m forgeMeta) (created bool, err error) {
	orgDesc := "Mirrors archived by matrix-archivist"
	if m.OrgURL != "" {
		orgDesc = "Mirrors of " + m.OrgURL + ", archived by matrix-archivist"
	}
	if err := a.mirror.EnsureOrg(ctx, owner, orgDesc, m.OrgURL, a.cfg.Mirror.Private); err != nil {
		return false, fmt.Errorf("creating organization %s: %w", owner, err)
	}
	r, err := a.mirror.GetRepo(ctx, owner, name)
	switch {
	case errors.Is(err, mirror.ErrNotFound):
		if err := a.mirror.CreateRepo(ctx, owner, name, m.Description, m.DefaultBranch, a.cfg.Mirror.Private); err != nil {
			return false, fmt.Errorf("creating repository: %w", err)
		}
		return true, nil
	case err != nil:
		return false, err
	case r.Archived:
		return false, a.mirror.SetArchived(ctx, owner, name, false)
	}
	return false, nil
}

func (a *Archiver) applyForgeMeta(ctx context.Context, owner, name string, m forgeMeta) error {
	// The default branch exists on the forge: it is in the archive (see mirrorMeta), and the
	// archive was pushed before the settings are applied.
	s := mirror.Settings{Description: m.Description, Website: m.Website, Private: m.Private, DefaultBranch: m.DefaultBranch}
	if err := a.mirror.EditRepo(ctx, owner, name, s); err != nil {
		return err
	}
	return a.mirror.SetTopics(ctx, owner, name, m.Topics)
}

// waitForgeNotEmpty waits until the forge has processed the push and no longer considers the
// repository empty (it refuses releases until then; Forgejo handles pushes in a background
// queue, so this can take a moment after the first push). ready is false if the archive has
// no branches, so the repository stays empty on the forge.
func (a *Archiver) waitForgeNotEmpty(ctx context.Context, owner, name string, refs gitops.Refs) (ready bool, err error) {
	hasBranch := false
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			hasBranch = true
			break
		}
	}
	deadline := time.Now().Add(a.mirrorEmptyWait)
	for {
		r, err := a.mirror.GetRepo(ctx, owner, name)
		if err != nil {
			return false, err
		}
		if !r.Empty {
			return true, nil
		}
		if !hasBranch {
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, errors.New("the forge still shows the repository as empty after the push (it may still be processing it); will retry")
		}
		live.From(ctx).SetStep("Waiting for the forge to process the push", "")
		select {
		case <-time.After(a.mirrorEmptyPoll):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// localRelease is an archived release as it is published on the forge.
type localRelease struct {
	Tag        string
	Title      string
	Body       string
	Published  time.Time
	Prerelease bool
	MetaOnly   bool
	Files      []localFile
}

type localFile struct {
	Name string
	Size int64
	Path string `json:"-"`
}

// localReleases reads the archived releases of a repository from disk, oldest first.
func (a *Archiver) localReleases(ctx context.Context, repo *store.Repo) []localRelease {
	archived, err := a.store.ArchivedReleases(ctx, repo.ID)
	if err != nil {
		return nil
	}
	sort.SliceStable(archived, func(i, j int) bool {
		if !archived[i].Published.Equal(archived[j].Published) {
			return archived[i].Published.Before(archived[j].Published)
		}
		return archived[i].Tag < archived[j].Tag
	})
	out := make([]localRelease, 0, len(archived))
	for _, ar := range archived {
		dir := a.releaseDir(repo, ar.Tag)
		l := localRelease{Tag: ar.Tag, Title: ar.Name, Published: ar.Published, MetaOnly: ar.MetaOnly}
		if b, err := os.ReadFile(filepath.Join(dir, "notes.md")); err == nil {
			title, body := parseNotes(string(b))
			if title != "" {
				l.Title = title
			}
			l.Body = body
		}
		if l.Title == "" {
			l.Title = ar.Tag
		}
		if b, err := os.ReadFile(filepath.Join(dir, "release.json")); err == nil {
			var raw struct {
				Prerelease bool `json:"prerelease"`
				Upcoming   bool `json:"upcoming_release"`
			}
			if json.Unmarshal(b, &raw) == nil {
				l.Prerelease = raw.Prerelease || raw.Upcoming
			}
		}
		if !ar.MetaOnly {
			entries, _ := os.ReadDir(filepath.Join(dir, "assets"))
			for _, e := range entries {
				if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), ".part") {
					continue
				}
				if info, err := e.Info(); err == nil {
					l.Files = append(l.Files, localFile{Name: e.Name(), Size: info.Size(), Path: filepath.Join(dir, "assets", e.Name())})
				}
			}
		}
		out = append(out, l)
	}
	return out
}

// parseNotes splits the notes.md written by saveRelease back into title and body.
func parseNotes(s string) (title, body string) {
	head, rest, ok := strings.Cut(s, "\n")
	if !ok || !strings.HasPrefix(head, "# ") {
		return "", strings.TrimSpace(s)
	}
	title = strings.TrimSpace(strings.TrimPrefix(head, "# "))
	// Skip the "Tag:" and "Published:" lines up to the blank line after them.
	for i := 0; i < 4; i++ {
		line, more, ok := strings.Cut(rest, "\n")
		if !ok {
			return title, ""
		}
		rest = more
		if strings.HasPrefix(line, "Published:") {
			break
		}
	}
	return title, strings.TrimSpace(strings.TrimPrefix(rest, "\n"))
}

// releaseBody is the release notes as shown on the forge, with a footer saying where the
// release comes from (the forge can't backdate a release).
func releaseBody(repo *store.Repo, l localRelease, files bool) string {
	var foot []string
	foot = append(foot, "Archived from "+repo.URL)
	if !l.Published.IsZero() {
		foot = append(foot, "originally published "+l.Published.UTC().Format("2006-01-02"))
	}
	text := "_" + strings.Join(foot, " · ") + "._"
	if l.MetaOnly && files {
		text += "  \n_The files of this release are not kept in the archive, only its notes._"
	}
	body := strings.TrimSpace(l.Body)
	if body == "" {
		return text
	}
	return body + "\n\n---\n" + text
}

// syncForgeReleases makes the forge's releases match the archive: missing ones are created,
// changed ones edited, files uploaded or removed, and releases the archive no longer has
// deleted (their tags stay). Releases whose tag isn't in the archive are left out.
func (a *Archiver) syncForgeReleases(ctx context.Context, repo *store.Repo, owner, name string, local []localRelease, refs gitops.Refs) (changed, files int, warnings []string, err error) {
	slot := live.From(ctx)
	slot.SetStep("Syncing releases to forge", "")
	remote, err := a.mirror.Releases(ctx, owner, name)
	if err != nil {
		return 0, 0, nil, err
	}
	byTag := map[string]mirror.Release{}
	for _, r := range remote {
		byTag[r.Tag] = r
	}
	withFiles := a.cfg.Mirror.ReleaseFiles
	missingTags := 0
	for i, l := range local {
		if ctx.Err() != nil {
			return changed, files, warnings, ctx.Err()
		}
		if _, ok := refs["refs/tags/"+l.Tag]; !ok {
			missingTags++
			continue
		}
		slot.SetStep("Syncing releases to forge", fmt.Sprintf("%s (%d/%d)", l.Tag, i+1, len(local)))
		opts := mirror.ReleaseOptions{Tag: l.Tag, Name: l.Title, Body: releaseBody(repo, l, withFiles), Prerelease: l.Prerelease}
		r, ok := byTag[l.Tag]
		delete(byTag, l.Tag)
		switch {
		case !ok:
			created, err := a.mirror.CreateRelease(ctx, owner, name, opts)
			if err != nil {
				return changed, files, warnings, fmt.Errorf("release %s: %w", l.Tag, err)
			}
			r = *created
			changed++
		case r.Name != opts.Name || strings.TrimSpace(r.Body) != strings.TrimSpace(opts.Body) || r.Prerelease != opts.Prerelease || r.Draft:
			if err := a.mirror.EditRelease(ctx, owner, name, r.ID, opts); err != nil {
				return changed, files, warnings, fmt.Errorf("release %s: %w", l.Tag, err)
			}
			changed++
		}
		if !withFiles {
			continue
		}
		want := map[string]localFile{}
		for _, f := range l.Files {
			want[f.Name] = f
		}
		have := map[string]bool{}
		for _, att := range r.Assets {
			if f, ok := want[att.Name]; ok && f.Size == att.Size && !have[att.Name] {
				have[att.Name] = true
				continue
			}
			if err := a.mirror.DeleteAttachment(ctx, owner, name, r.ID, att.ID); err != nil {
				return changed, files, warnings, fmt.Errorf("release %s: deleting %s: %w", l.Tag, att.Name, err)
			}
			files++
		}
		for j, f := range l.Files {
			if have[f.Name] {
				continue
			}
			slot.SetStep("Uploading release file to forge", fmt.Sprintf("%s · %d/%d: %s (%s)", l.Tag, j+1, len(l.Files), f.Name, gitops.HumanSize(f.Size)))
			if err := a.mirror.UploadAttachment(ctx, owner, name, r.ID, f.Name, f.Path); err != nil {
				var apiErr *mirror.APIError
				if errors.As(err, &apiErr) && apiErr.Status < 500 {
					// Refused by the forge (too large, type not allowed): later syncs won't
					// change that, so it's a warning rather than a failure.
					warnings = append(warnings, fmt.Sprintf("release %s: %s refused: %s", l.Tag, f.Name, oneLine(apiErr.Message)))
					continue
				}
				return changed, files, warnings, fmt.Errorf("release %s: uploading %s: %w", l.Tag, f.Name, err)
			}
			files++
		}
	}
	for _, r := range byTag {
		if err := a.mirror.DeleteRelease(ctx, owner, name, r.ID); err != nil {
			return changed, files, warnings, fmt.Errorf("deleting release %s: %w", r.Tag, err)
		}
		changed++
	}
	if missingTags > 0 {
		a.log.Debug().Int64("repo_id", repo.ID).Int("releases", missingTags).Msg("Releases without their tag in the archive were not published")
	}
	return changed, files, warnings, nil
}

// unpublishMirror archives (read-only) or, with purge, deletes a removed repository's copy on
// the forge. Errors are logged only.
func (a *Archiver) unpublishMirror(ctx context.Context, repo *store.Repo, path string, purge bool) {
	if a.mirror == nil || path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	owner, name, _ := strings.Cut(path, "/")
	var err error
	if purge {
		err = a.mirror.DeleteRepo(ctx, owner, name)
		if errors.Is(err, mirror.ErrNotFound) {
			err = nil
		}
	} else {
		err = a.mirror.SetArchived(ctx, owner, name, true)
	}
	if err != nil {
		a.log.Warn().Err(err).Str("forge_repo", path).Msg("Failed to update the removed repository on the forge")
		return
	}
	what := "archived (read-only)"
	if purge {
		what = "deleted"
	}
	a.Live.Event(repo.ID, repo.URL, "mirror", "Forge copy "+what+": "+path)
}

// MirrorOverview counts the repositories by forge sync state.
type MirrorOverview struct {
	Archived, Synced, Pending, Failing, Warnings int
	Failures                                     []MirrorFailure
}

type MirrorFailure struct {
	Repo  *store.Repo
	State store.MirrorState
}

// MirrorOverview summarizes the forge sync of every archived repository.
func (a *Archiver) MirrorOverview(ctx context.Context) (o MirrorOverview, err error) {
	repos, err := a.store.Repos(ctx)
	if err != nil {
		return o, err
	}
	states, err := a.store.MirrorStates(ctx)
	if err != nil {
		return o, err
	}
	for _, r := range repos {
		if !r.Cloned {
			continue
		}
		o.Archived++
		st, ok := states[r.ID]
		switch {
		case ok && st.FailCount > 0:
			o.Failing++
			o.Failures = append(o.Failures, MirrorFailure{r, st})
		case ok && !st.SyncedAt.IsZero():
			o.Synced++
			if st.Error != "" {
				o.Warnings++
			}
		default:
			o.Pending++
		}
	}
	return o, nil
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
