// Package archiver keeps local archives of git repositories and their releases up to date.
package archiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/forge"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// Notifier posts a Markdown message to the room.
type Notifier func(ctx context.Context, markdown string)

type Archiver struct {
	cfg   *config.Config
	store *store.Store
	git   *gitops.Git
	forge *forge.Client
	log   zerolog.Logger

	// Notify posts an informational message; Alert posts one that pings the whole room (@room).
	Notify Notifier
	Alert  Notifier

	locksMu sync.Mutex
	locks   map[int64]*sync.Mutex

	cycleRunning atomic.Bool

	// Counters exposed as metrics.
	checksOK, checksFailed atomic.Int64
	lastCycleEnd           atomic.Int64 // unix seconds
	lastCycleDur           atomic.Int64 // milliseconds
}

func New(cfg *config.Config, st *store.Store, log zerolog.Logger) *Archiver {
	return &Archiver{
		cfg:    cfg,
		store:  st,
		git:    gitops.New(),
		forge:  forge.NewClient(),
		log:    log,
		locks:  map[int64]*sync.Mutex{},
		Notify: func(context.Context, string) {},
		Alert:  func(context.Context, string) {},
	}
}

func (a *Archiver) lock(id int64) *sync.Mutex {
	a.locksMu.Lock()
	defer a.locksMu.Unlock()
	m, ok := a.locks[id]
	if !ok {
		m = &sync.Mutex{}
		a.locks[id] = m
	}
	return m
}

// Paths for a repository's storage.
func (a *Archiver) RepoDir(r *store.Repo) string {
	return filepath.Join(a.cfg.StorageDir, filepath.FromSlash(r.RelDir))
}
func (a *Archiver) mirrorDir(r *store.Repo) string   { return filepath.Join(a.RepoDir(r), "mirror.git") }
func (a *Archiver) checkoutDir(r *store.Repo) string { return filepath.Join(a.RepoDir(r), "checkout") }
func (a *Archiver) releasesDir(r *store.Repo) string { return filepath.Join(a.RepoDir(r), "releases") }

// Result of checking one repository.
type Result struct {
	Repo        *store.Repo
	Cloned      bool // first clone happened in this check
	Change      gitops.Change
	NewReleases []string
	Err         error // git error (fatal for this check)
	Warnings    []string
	Preserved   int // refs saved under refs/archive because upstream rewrote/deleted them

	url    gitops.RepoURL
	urlOK  bool
	branch string // default branch after the check
}

func (r Result) Changed() bool { return r.Cloned || !r.Change.Empty() || len(r.NewReleases) > 0 }

// Text is a one-line Markdown description of the result.
func (r Result) Text() string {
	if r.Err != nil {
		return fmt.Sprintf("❌ failed: `%s`", oneLine(r.Err.Error()))
	}
	var parts []string
	if r.Cloned {
		parts = append(parts, "cloned")
	} else if !r.Change.Empty() {
		parts = append(parts, r.Change.Summary())
	}
	if n := len(r.NewReleases); n > 0 {
		parts = append(parts, releasesText(r.NewReleases))
	}
	if len(parts) == 0 {
		parts = append(parts, "up to date")
	}
	s := strings.Join(parts, "; ")
	if len(r.Warnings) > 0 {
		s += fmt.Sprintf(" (⚠️ %s)", strings.Join(r.Warnings, "; "))
	}
	return s
}

func releasesText(tags []string) string {
	if len(tags) == 1 {
		return "new release " + tags[0]
	}
	if len(tags) <= 5 {
		return fmt.Sprintf("%d new releases (%s)", len(tags), strings.Join(tags, ", "))
	}
	return fmt.Sprintf("%d new releases (%s, +%d more)", len(tags), strings.Join(tags[:5], ", "), len(tags)-5)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "`", "'")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Check fetches updates for one repository (cloning it first if needed) and archives new releases.
func (a *Archiver) Check(ctx context.Context, id int64) Result {
	m := a.lock(id)
	m.Lock()
	defer m.Unlock()

	repo, err := a.store.Repo(ctx, id)
	if err != nil {
		return Result{Err: err}
	}
	res := Result{Repo: repo}
	log := a.log.With().Int64("repo_id", repo.ID).Str("url", repo.URL).Logger()

	u, err := gitops.ParseRepoURL(repo.URL)
	if err != nil {
		res.Err = err
		return a.finish(ctx, res, log)
	}
	res.url, res.urlOK = u, true

	gctx, cancel := context.WithTimeout(ctx, a.cfg.GitTimeout)
	defer cancel()

	mirror := a.mirrorDir(repo)
	if _, statErr := os.Stat(mirror); errors.Is(statErr, os.ErrNotExist) {
		log.Info().Msg("Cloning repository")
		if err := a.git.CloneMirror(gctx, repo.URL, mirror); err != nil {
			res.Err = err
			return a.finish(ctx, res, log)
		}
		res.Cloned = true
	} else {
		before, err := a.git.SnapshotRefs(gctx, mirror)
		if err != nil {
			res.Err = err
			return a.finish(ctx, res, log)
		}
		if err := a.git.FetchMirror(gctx, mirror, a.cfg.PruneDeleted); err != nil {
			res.Err = err
			return a.finish(ctx, res, log)
		}
		after, err := a.git.SnapshotRefs(gctx, mirror)
		if err != nil {
			res.Err = err
			return a.finish(ctx, res, log)
		}
		res.Change = a.git.Diff(gctx, mirror, before, after)
		if res.Change.Destructive() {
			stamp := time.Now().UTC().Format("20060102T150405Z")
			n, err := a.git.PreserveRefs(gctx, mirror, before, &res.Change, stamp)
			if err != nil {
				log.Err(err).Msg("Failed to preserve rewritten refs")
				res.Warnings = append(res.Warnings, "preserving old refs: "+oneLine(err.Error()))
			} else {
				res.Preserved = n
				log.Warn().Str("snapshot", res.Change.Snapshot).Int("refs", n).Msg("Upstream rewrote or deleted refs; old tips preserved")
			}
		}
	}
	res.branch = a.git.DefaultBranch(gctx, mirror)

	if res.Cloned || !res.Change.Empty() || !dirExists(a.checkoutDir(repo)) {
		if err := a.git.SyncCheckout(gctx, mirror, a.checkoutDir(repo)); err != nil {
			res.Warnings = append(res.Warnings, "checkout: "+oneLine(err.Error()))
		}
	}

	if a.cfg.Releases.Enabled {
		tags, warns := a.archiveReleases(ctx, repo, u, log)
		res.NewReleases = tags
		res.Warnings = append(res.Warnings, warns...)
	}
	return a.finish(ctx, res, log)
}

func (a *Archiver) finish(ctx context.Context, res Result, log zerolog.Logger) Result {
	repo := res.Repo
	wasFailing := repo.FailCount > 0
	if err := a.store.MarkChecked(ctx, repo.ID, res.Changed(), res.Err); err != nil {
		log.Err(err).Msg("Failed to save check result")
	}
	switch {
	case res.Err != nil:
		a.checksFailed.Add(1)
		log.Warn().Err(res.Err).Msg("Check failed")
		_ = a.store.AddEvent(ctx, repo.ID, store.EventError, oneLine(res.Err.Error()))
		alerted := repo.Upstream != ""
		// Only archived repos can "disappear"; a never-cloned one is just a bad URL.
		if st := a.probeUpstream(ctx, res); repo.Cloned && st.State == forge.Gone || st.State == forge.Legal || st.State == forge.Blocked {
			_ = a.store.SetUpstream(ctx, repo.ID, st.State, st.Detail)
			if repo.Upstream != st.State {
				log.Warn().Str("state", st.State).Str("detail", st.Detail).Msg("Repository disappeared upstream")
				_ = a.store.AddEvent(ctx, repo.ID, store.EventGone, st.State+": "+st.Detail)
				nrel, _ := a.store.ReleaseCount(ctx, repo.ID)
				a.Alert(ctx, a.GoneAlertText(repo, st, nrel))
			}
			alerted = true
		}
		if n := repo.FailCount + 1; !alerted && a.cfg.FailureAlert > 0 && n == a.cfg.FailureAlert {
			a.Alert(ctx, FailureAlertText(repo, n, res.Err))
		}
	default:
		a.checksOK.Add(1)
		if repo.Upstream != "" {
			_ = a.store.SetUpstream(ctx, repo.ID, "", "")
			_ = a.store.AddEvent(ctx, repo.ID, store.EventRecovered, "back upstream")
			a.Notify(ctx, fmt.Sprintf("♻️ **#%d** %s is reachable upstream again.", repo.ID, repo.URL))
		} else if wasFailing {
			_ = a.store.AddEvent(ctx, repo.ID, store.EventRecovered, "recovered")
			// Only announce recovery if the room was alerted about the failure.
			if a.cfg.FailureAlert > 0 && repo.FailCount >= a.cfg.FailureAlert {
				a.Notify(ctx, fmt.Sprintf("✅ **#%d** %s is working again after %d failed checks.", repo.ID, repo.URL, repo.FailCount))
			}
		}
		if res.Changed() || repo.SizeBytes == 0 {
			_ = a.store.SetSize(ctx, repo.ID, gitops.DirSize(a.RepoDir(repo)))
		}
		if res.Cloned {
			detail := "initial clone"
			if len(res.NewReleases) > 0 {
				detail += fmt.Sprintf(", %d releases archived", len(res.NewReleases))
			}
			_ = a.store.AddEvent(ctx, repo.ID, store.EventAdded, detail)
		} else {
			if !res.Change.Empty() {
				detail, _ := json.Marshal(res.Change)
				_ = a.store.AddEvent(ctx, repo.ID, store.EventUpdate, string(detail))
			}
			for _, t := range res.NewReleases {
				_ = a.store.AddEvent(ctx, repo.ID, store.EventRelease, t)
			}
		}
		if text := RewriteNoticeText(repo, res.Change, res.branch); text != "" {
			a.Notify(ctx, text)
		}
		log.Debug().Str("result", res.Text()).Msg("Checked repository")
	}
	if r, err := a.store.Repo(ctx, repo.ID); err == nil {
		res.Repo = r
	}
	return res
}

// probeUpstream asks the forge (or the repository's web page) whether a failing repository
// still exists, and falls back to recognising takedown messages in git's error.
func (a *Archiver) probeUpstream(ctx context.Context, res Result) forge.RepoStatus {
	if !res.urlOK || !res.Repo.Cloned || ctx.Err() != nil {
		return forge.RepoStatus{State: forge.Unknown}
	}
	u := res.url
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st := a.forge.Probe(pctx, a.provider(pctx, u.Host), u.Owner, u.Name, "https://"+u.Host+"/"+u.Path)
	if st.State == forge.Unknown {
		if g := forge.ClassifyGitError(res.Err.Error()); g.State != forge.Unknown {
			return g
		}
	}
	return st
}

// GoneAlertText is the @room alert sent when a repository disappears upstream.
func (a *Archiver) GoneAlertText(repo *store.Repo, st forge.RepoStatus, releases int) string {
	icon, what := "🪦", "has disappeared upstream"
	switch st.State {
	case forge.Legal:
		icon, what = "⚖️", "has been taken down upstream"
	case forge.Blocked:
		icon, what = "🚫", "has been blocked upstream"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s **#%d** %s %s: %s\n\n", icon, repo.ID, repo.URL, what, st.Detail)
	last := "never"
	if !repo.LastChanged.IsZero() {
		last = repo.LastChanged.In(a.cfg.Summary.Location).Format("2006-01-02 15:04 MST")
	}
	fmt.Fprintf(&b, "The archive is safe: %s, %d releases, last change synced %s.", gitops.HumanSize(repo.SizeBytes), releases, last)
	if u := a.cfg.HTTP.BrowseURL(repo.RelDir); u != "" {
		fmt.Fprintf(&b, " Browse: %s", u)
	}
	return b.String()
}

// RewriteNoticeText describes destructive upstream changes worth telling the room about right
// away: history rewritten on the default branch, or tags moved/deleted. Rewrites of other
// branches only appear in the summary. Returns "" if there is nothing to announce.
func RewriteNoticeText(repo *store.Repo, c gitops.Change, defaultBranch string) string {
	var what []string
	for _, br := range c.RewrittenBranches {
		if br == defaultBranch {
			what = append(what, fmt.Sprintf("history of `%s` was rewritten (force push)", br))
		}
	}
	if len(c.MovedTags) > 0 {
		what = append(what, fmt.Sprintf("tags were moved: %s", strings.Join(c.MovedTags, ", ")))
	}
	if len(c.DeletedTags) > 0 {
		what = append(what, fmt.Sprintf("tags were deleted: %s", strings.Join(c.DeletedTags, ", ")))
	}
	if len(what) == 0 {
		return ""
	}
	text := fmt.Sprintf("⚠️ **#%d** %s: upstream %s.", repo.ID, repo.URL, strings.Join(what, "; "))
	if c.Snapshot != "" {
		text += fmt.Sprintf(" The previous state is preserved in `mirror.git` under `%s/`.", c.Snapshot)
	}
	return text
}

// FailureAlertText is the body of the @room alert sent when a repo hits the failure threshold.
func FailureAlertText(repo *store.Repo, failures int, err error) string {
	return fmt.Sprintf("🚨 **#%d** %s has failed %d times in a row.\n\nLast error: `%s`",
		repo.ID, repo.URL, failures, oneLine(err.Error()))
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// provider resolves the release provider for a host; nil if none.
func (a *Archiver) provider(ctx context.Context, host string) forge.Provider {
	if f, ok := a.cfg.ForgeFor(host); ok {
		p, err := a.forge.New(f.Type, host, f.APIURL, f.Token)
		if err != nil {
			return nil
		}
		return p
	}
	typ := forge.KnownHostType(host)
	if typ == "" {
		var cached bool
		typ, cached = a.store.ForgeType(ctx, host, 7*24*time.Hour)
		if !cached {
			typ = a.forge.Detect(ctx, host)
			_ = a.store.SetForgeType(ctx, host, typ)
			a.log.Info().Str("host", host).Str("type", typ).Msg("Detected forge type")
		}
	}
	if typ == "" {
		return nil
	}
	p, _ := a.forge.New(typ, host, "", "")
	return p
}

func (a *Archiver) archiveReleases(ctx context.Context, repo *store.Repo, u gitops.RepoURL, log zerolog.Logger) (newTags, warnings []string) {
	if u.Owner == "" {
		return nil, nil
	}
	p := a.provider(ctx, u.Host)
	if p == nil {
		return nil, nil
	}
	rels, err := p.ListReleases(ctx, u.Owner, u.Name)
	if errors.Is(err, forge.ErrNoRepo) {
		return nil, nil
	} else if err != nil {
		log.Warn().Err(err).Msg("Failed to list releases")
		return nil, []string{"releases: " + oneLine(err.Error())}
	}
	for _, rel := range rels {
		if rel.Draft || rel.Tag == "" || (rel.Prerelease && !a.cfg.Releases.IncludePrereleases) {
			continue
		}
		if done, err := a.store.HasRelease(ctx, repo.ID, rel.Tag); err != nil || done {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		dir := filepath.Join(a.releasesDir(repo), forge.SafeName(rel.Tag))
		complete, skipped, err := a.saveRelease(ctx, rel, dir)
		if err != nil {
			log.Warn().Err(err).Str("tag", rel.Tag).Msg("Failed to archive release")
			warnings = append(warnings, fmt.Sprintf("release %s: %s", rel.Tag, oneLine(err.Error())))
			continue
		}
		if len(skipped) > 0 {
			warnings = append(warnings, fmt.Sprintf("release %s: skipped oversized assets %s", rel.Tag, strings.Join(skipped, ", ")))
		}
		if complete {
			_ = a.store.AddRelease(ctx, repo.ID, rel.Tag, rel.Name, rel.Published)
			newTags = append(newTags, rel.Tag)
			log.Info().Str("tag", rel.Tag).Msg("Archived release")
		}
	}
	return newTags, warnings
}

// saveRelease writes metadata and downloads assets. complete is false if some asset failed
// in a retryable way (the release will be retried on the next check).
func (a *Archiver) saveRelease(ctx context.Context, rel forge.Release, dir string) (complete bool, skipped []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, nil, err
	}
	if len(rel.Raw) > 0 {
		var pretty json.RawMessage = rel.Raw
		if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			pretty = b
		}
		if err := os.WriteFile(filepath.Join(dir, "release.json"), pretty, 0o644); err != nil {
			return false, nil, err
		}
	}
	title := rel.Name
	if title == "" {
		title = rel.Tag
	}
	notes := fmt.Sprintf("# %s\n\nTag: `%s`  \nPublished: %s\n\n%s\n", title, rel.Tag, rel.Published.Format(time.RFC3339), rel.Body)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(notes), 0o644); err != nil {
		return false, nil, err
	}
	if !a.cfg.Releases.DownloadAssets {
		return true, nil, nil
	}
	complete = true
	var firstErr error
	for _, asset := range rel.Assets {
		_, err := a.forge.Download(ctx, asset, filepath.Join(dir, "assets"), a.cfg.Releases.MaxAssetSize)
		switch {
		case err == nil:
		case errors.Is(err, forge.ErrTooLarge):
			skipped = append(skipped, asset.Name)
		default:
			complete = false
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return complete, skipped, firstErr
}

// RunCycle checks all repositories with bounded concurrency. Returns false if a cycle was
// already running.
func (a *Archiver) RunCycle(ctx context.Context) (results []Result, ran bool) {
	if !a.cycleRunning.CompareAndSwap(false, true) {
		return nil, false
	}
	defer a.cycleRunning.Store(false)

	repos, err := a.store.Repos(ctx)
	if err != nil {
		a.log.Err(err).Msg("Failed to list repos")
		return nil, true
	}
	start := time.Now()
	a.log.Info().Int("repos", len(repos)).Msg("Starting check cycle")
	ids := make([]int64, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	results = a.CheckMany(ctx, ids)
	took := time.Since(start)
	a.lastCycleEnd.Store(time.Now().Unix())
	a.lastCycleDur.Store(took.Milliseconds())
	a.log.Info().Int("repos", len(results)).Dur("took", took).Msg("Check cycle finished")
	a.CheckDisk(ctx)
	return results, true
}

// CheckMany checks the given repositories with bounded concurrency.
func (a *Archiver) CheckMany(ctx context.Context, ids []int64) (results []Result) {
	jobs := make(chan int64)
	out := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < a.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				out <- a.Check(ctx, id)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(out) }()
	for r := range out {
		results = append(results, r)
	}
	return results
}

// Run executes the periodic check loop and summary scheduler until ctx is cancelled.
func (a *Archiver) Run(ctx context.Context) {
	go a.runSummaries(ctx)

	// Give the bot a moment to connect before the first cycle.
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(a.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		a.RunCycle(ctx)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// Remove deletes a repo from the list and optionally its files on disk.
func (a *Archiver) Remove(ctx context.Context, repo *store.Repo, purge bool) error {
	m := a.lock(repo.ID)
	m.Lock()
	defer m.Unlock()
	if err := a.store.RemoveRepo(ctx, repo.ID); err != nil {
		return err
	}
	if purge {
		if inUse, _ := a.store.RelDirInUse(ctx, repo.RelDir, repo.ID); !inUse {
			dir := a.RepoDir(repo)
			// Defensive: never delete outside the storage dir.
			if rel, err := filepath.Rel(a.cfg.StorageDir, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				return os.RemoveAll(dir)
			}
		}
	}
	return nil
}
