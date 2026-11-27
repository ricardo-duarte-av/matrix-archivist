// Package archiver keeps local archives of git repositories and their releases up to date.
package archiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/forge"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/live"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/mirror"
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

	// sourceInvalid remembers each source's invalid URLs, to log them only when they change.
	sourceInvalid map[string]string
	// sourceKicks holds one channel per source; a send makes it sync now (see TriggerSource).
	sourceKicks map[string]chan struct{}

	// Live is what the archiver is doing right now, for the live activity page.
	Live *live.Hub

	// mirror publishes repositories to a forge (nil when disabled).
	mirror        *mirror.Client
	mirrorKick    chan struct{}
	mirrorReady   atomic.Bool // the forge accepted the token
	mirrorRunning atomic.Bool
	// How long and how often to wait for the forge to process a push (see waitForgeNotEmpty).
	mirrorEmptyWait, mirrorEmptyPoll time.Duration

	// Counters exposed as metrics.
	checksOK, checksFailed atomic.Int64
	lastCycleEnd           atomic.Int64 // unix seconds
	lastCycleDur           atomic.Int64 // milliseconds
}

func New(cfg *config.Config, st *store.Store, log zerolog.Logger) *Archiver {
	fc := forge.NewClient()
	if !cfg.AllowInternalHosts {
		fc.BlockInternal()
	}
	hub := live.New(cfg.Concurrency)
	hub.Disk = func() (uint64, uint64, error) { return FreeSpace(cfg.StorageDir) }
	a := &Archiver{
		cfg:           cfg,
		store:         st,
		git:           gitops.New(),
		forge:         fc,
		log:           log,
		locks:         map[int64]*sync.Mutex{},
		sourceInvalid: map[string]string{},
		sourceKicks:   map[string]chan struct{}{},
		Notify:        func(context.Context, string) {},
		Alert:         func(context.Context, string) {},
		Live:          hub,
	}
	for _, src := range cfg.Sources {
		a.sourceKicks[src.Name] = make(chan struct{}, 1)
	}
	if cfg.Mirror.Enabled {
		a.mirror = mirror.NewClient(cfg.Mirror.URL, cfg.Mirror.Token)
		a.mirrorKick = make(chan struct{}, 1)
		a.mirrorEmptyWait, a.mirrorEmptyPoll = 3*time.Minute, 2*time.Second
	}
	return a
}

// Annotate fills in Activity for repositories that are being checked right now.
func (a *Archiver) Annotate(repos ...*store.Repo) {
	for _, r := range repos {
		r.Activity, r.ActivitySince = a.Live.Activity(r.ID)
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
	// PrunedReleases were removed to respect releases.keep_last / max_repo_size.
	PrunedReleases []string
	Err            error // git error (fatal for this check)
	Warnings       []string
	Preserved      int // refs saved under refs/archive because upstream rewrote/deleted them
	// StillGone is set when the repo was already flagged gone upstream and a probe found it
	// still missing, so the fetch was skipped (no error, no fail count, quiet feed).
	StillGone string
	// FilledReleases already had their notes archived and now got their files too (the
	// repository switched to full mode, or its limits were raised).
	FilledReleases []string
	// Checkout is "created" or "removed" when a mode switch added or dropped the checkout.
	Checkout string

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
	if r.StillGone != "" {
		return fmt.Sprintf("🪦 still %s upstream; fetch skipped, archive preserved", goneWord(r.StillGone))
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
	if r.Checkout != "" {
		parts = append(parts, "checkout "+r.Checkout)
	}
	if n := len(r.FilledReleases); n > 0 {
		parts = append(parts, fmt.Sprintf("downloaded the files of %d earlier release(s)", n))
	}
	if n := len(r.PrunedReleases); n > 0 {
		if r.Repo != nil && r.Repo.Mode == store.ModeLight {
			parts = append(parts, fmt.Sprintf("removed the files of %d older release(s), keeping their notes", n))
		} else {
			parts = append(parts, fmt.Sprintf("removed %d old release(s) to stay within the release limits", n))
		}
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
	return a.check(ctx, id)
}

// check does the work of Check; the caller holds the repo's lock.
func (a *Archiver) check(ctx context.Context, id int64) Result {
	slot := live.From(ctx)
	if slot == nil {
		slot = a.Live.Temp("Manual check")
		ctx = live.WithSlot(ctx, slot)
	}
	defer slot.End()

	repo, err := a.store.Repo(ctx, id)
	if err != nil {
		return Result{Err: err}
	}
	slot.Begin(repo.ID, repo.URL, oneLine(repo.About()))
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
	// A repo already known to be gone upstream: probe first and only fetch once it's back,
	// instead of failing (and filling the feed) every cycle.
	if repo.Upstream != "" && repo.Cloned && dirExists(mirror) {
		if st := a.probe(ctx, u); isGone(st.State) {
			res.StillGone = st.State
			return a.finishGone(ctx, res, st, log)
		}
	}
	if _, statErr := os.Stat(mirror); errors.Is(statErr, os.ErrNotExist) {
		log.Info().Msg("Cloning repository")
		slot.SetStep("Cloning", "")
		if err := a.git.CloneMirror(gctx, repo.URL, mirror); err != nil {
			res.Err = err
			return a.finish(ctx, res, log)
		}
		res.Cloned = true
		_ = a.store.MarkCloned(ctx, id)
	} else {
		slot.SetStep("Fetching", "")
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
			slot.SetStep("Preserving rewritten history", "")
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

	if repo.Mode == store.ModeLight {
		// Light archives keep only the mirror; drop a checkout left from full mode.
		if dirExists(a.checkoutDir(repo)) {
			if err := os.RemoveAll(a.checkoutDir(repo)); err != nil {
				res.Warnings = append(res.Warnings, "removing checkout: "+oneLine(err.Error()))
			}
			res.Checkout = "removed"
		}
	} else if res.Cloned || !res.Change.Empty() || !dirExists(a.checkoutDir(repo)) {
		if !res.Cloned && !dirExists(a.checkoutDir(repo)) {
			res.Checkout = "created"
		}
		slot.SetStep("Updating checkout", "")
		if err := a.git.SyncCheckout(gctx, mirror, a.checkoutDir(repo)); err != nil {
			res.Warnings = append(res.Warnings, "checkout: "+oneLine(err.Error()))
		}
	}

	if a.cfg.Releases.Enabled {
		rr := a.archiveReleases(ctx, repo, u, log)
		res.NewReleases, res.FilledReleases, res.PrunedReleases = rr.New, rr.Filled, rr.Pruned
		res.Warnings = append(res.Warnings, rr.Warnings...)
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
		a.Live.Event(repo.ID, repo.URL, "error", "Check failed: "+oneLine(res.Err.Error()))
		alerted := repo.Upstream != ""
		// Only archived repos can "disappear"; a never-cloned one is just a bad URL.
		if st := a.probeUpstream(ctx, res); repo.Cloned && isGone(st.State) {
			_ = a.store.SetUpstream(ctx, repo.ID, st.State, st.Detail)
			if repo.Upstream != st.State {
				log.Warn().Str("state", st.State).Str("detail", st.Detail).Msg("Repository disappeared upstream")
				_ = a.store.AddEvent(ctx, repo.ID, store.EventGone, st.State+": "+st.Detail)
				a.Live.Event(repo.ID, repo.URL, "gone", "Disappeared upstream ("+st.State+"): "+st.Detail+"; archive preserved")
				nrel, _ := a.store.ReleaseCount(ctx, repo.ID)
				a.Alert(ctx, a.GoneAlertText(repo, st, nrel))
			}
			alerted = true
		}
		// Archives of repos that vanished upstream still get their README indexed.
		a.updateMeta(ctx, res, false, log)
		// Light repositories (typically added in bulk by a source) only show failures in the
		// summary; disappearing upstream is still alerted above.
		if n := repo.FailCount + 1; !alerted && repo.Mode != store.ModeLight && a.cfg.FailureAlert > 0 && n == a.cfg.FailureAlert {
			a.Alert(ctx, FailureAlertText(repo, n, res.Err))
		}
	default:
		a.checksOK.Add(1)
		if repo.Upstream != "" {
			_ = a.store.SetUpstream(ctx, repo.ID, "", "")
			_ = a.store.AddEvent(ctx, repo.ID, store.EventRecovered, "back upstream")
			a.Live.Event(repo.ID, repo.URL, "recovered", "Reachable upstream again")
			a.Notify(ctx, fmt.Sprintf("♻️ **#%d** %s is reachable upstream again.", repo.ID, repo.URL))
		} else if wasFailing {
			_ = a.store.AddEvent(ctx, repo.ID, store.EventRecovered, "recovered")
			// Only announce recovery if the room was alerted about the failure.
			if repo.Mode != store.ModeLight && a.cfg.FailureAlert > 0 && repo.FailCount >= a.cfg.FailureAlert {
				a.Notify(ctx, fmt.Sprintf("✅ **#%d** %s is working again after %d failed checks.", repo.ID, repo.URL, repo.FailCount))
			}
		}
		if res.Changed() || len(res.PrunedReleases) > 0 || len(res.FilledReleases) > 0 || res.Checkout != "" || repo.SizeBytes == 0 {
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
		a.updateMeta(ctx, res, true, log)
		switch {
		case !res.Change.Empty() || res.Cloned:
			a.Live.Event(repo.ID, repo.URL, "change", res.Text())
		case len(res.NewReleases) > 0:
			a.Live.Event(repo.ID, repo.URL, "release", res.Text())
		case len(res.PrunedReleases) > 0:
			a.Live.Event(repo.ID, repo.URL, "prune", res.Text())
		}
		// Rewrites in light repositories are preserved the same way but only reported in
		// the summary.
		if text := RewriteNoticeText(repo, res.Change, res.branch); text != "" && repo.Mode != store.ModeLight {
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
	st := a.probe(ctx, res.url)
	if st.State == forge.Unknown {
		if g := forge.ClassifyGitError(res.Err.Error()); g.State != forge.Unknown {
			return g
		}
	}
	return st
}

// probe asks the forge (or the repo's web page) whether the repository exists.
func (a *Archiver) probe(ctx context.Context, u gitops.RepoURL) forge.RepoStatus {
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	live.From(ctx).SetStep("Checking upstream", "")
	return a.forge.Probe(pctx, a.provider(pctx, u.Host), u.Owner, u.Name, "https://"+u.Host+"/"+u.Path)
}

func isGone(state string) bool {
	return state == forge.Gone || state == forge.Legal || state == forge.Blocked
}

func goneWord(state string) string {
	switch state {
	case forge.Legal:
		return "taken down"
	case forge.Blocked:
		return "blocked"
	}
	return "gone"
}

// finishGone records a skipped check of a repo that is still missing upstream. The fail
// count and last error stay as they were; only a change of reason (e.g. gone → DMCA) is logged.
func (a *Archiver) finishGone(ctx context.Context, res Result, st forge.RepoStatus, log zerolog.Logger) Result {
	repo := res.Repo
	_ = a.store.TouchChecked(ctx, repo.ID)
	if st.State != repo.Upstream {
		_ = a.store.SetUpstream(ctx, repo.ID, st.State, st.Detail)
		_ = a.store.AddEvent(ctx, repo.ID, store.EventGone, st.State+": "+st.Detail)
		a.Live.Event(repo.ID, repo.URL, "gone", "Still missing upstream, now "+st.State+": "+st.Detail)
	}
	log.Debug().Str("state", st.State).Msg("Still gone upstream; fetch skipped")
	if r, err := a.store.Repo(ctx, repo.ID); err == nil {
		res.Repo = r
	}
	return res
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

// releaseResult is what archiveReleases did.
type releaseResult struct {
	New      []string // releases archived for the first time
	Filled   []string // releases that had notes only and got their files
	Pruned   []string // releases removed (or reduced to notes) by the retention limits
	Warnings []string
}

func (a *Archiver) archiveReleases(ctx context.Context, repo *store.Repo, u gitops.RepoURL, log zerolog.Logger) (out releaseResult) {
	if u.Owner == "" {
		return out
	}
	p := a.provider(ctx, u.Host)
	if p == nil {
		return out
	}
	slot := live.From(ctx)
	slot.SetStep("Listing releases", "")
	rels, err := p.ListReleases(ctx, u.Owner, u.Name)
	if errors.Is(err, forge.ErrNoRepo) {
		return out
	} else if err != nil {
		log.Warn().Err(err).Msg("Failed to list releases")
		out.Warnings = []string{"releases: " + oneLine(err.Error())}
		return out
	}
	pol := a.releasePolicy(repo)
	cands, err := a.releaseCandidates(ctx, repo, pol, rels)
	if err != nil {
		out.Warnings = []string{"releases: " + oneLine(err.Error())}
		return out
	}
	// Releases to download with their files: in the plan and not archived with them yet.
	// In light mode, every other release not archived yet gets its notes only.
	type job struct {
		c     ReleaseCandidate
		files bool
	}
	var todo []job
	planned := map[string]bool{}
	for _, c := range a.planReleases(repo, pol, cands) {
		planned[c.Tag] = true
		if (!c.Archived || c.MetaOnly || c.Incomplete) && c.Release != nil {
			todo = append(todo, job{c, true})
		}
	}
	if pol.allNotes {
		for _, c := range cands {
			if !planned[c.Tag] && !c.Archived && c.Release != nil {
				todo = append(todo, job{c, false})
			}
		}
	}
	for i, j := range todo {
		if ctx.Err() != nil {
			break
		}
		rel := *j.c.Release
		label := fmt.Sprintf("%s (%d/%d)", rel.Tag, i+1, len(todo))
		slot.SetStep("Archiving release", label)
		dir := filepath.Join(a.releasesDir(repo), forge.SafeName(rel.Tag))
		files := j.files && pol.downloadAssets
		complete, skipped, err := a.saveRelease(ctx, rel, dir, label, files, pol.maxAssetSize)
		if err != nil {
			log.Warn().Err(err).Str("tag", rel.Tag).Msg("Failed to archive release")
			out.Warnings = append(out.Warnings, fmt.Sprintf("release %s: %s", rel.Tag, oneLine(err.Error())))
			continue
		}
		if len(skipped) > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("release %s: skipped oversized assets %s", rel.Tag, strings.Join(skipped, ", ")))
		}
		if complete {
			_ = a.store.AddRelease(ctx, repo.ID, rel.Tag, rel.Name, rel.Published, pol.allNotes && !files)
			if !j.c.Archived {
				out.New = append(out.New, rel.Tag)
			} else if files {
				out.Filled = append(out.Filled, rel.Tag)
			}
			log.Info().Str("tag", rel.Tag).Msg("Archived release")
		}
	}
	if (pol.limited() || pol.allNotes) && ctx.Err() == nil {
		// Re-plan with the real sizes now on disk and remove what no longer fits.
		slot.SetStep("Applying release limits", "")
		pruned, err := a.pruneReleases(ctx, repo, pol, rels, log)
		if err != nil {
			out.Warnings = append(out.Warnings, "pruning releases: "+oneLine(err.Error()))
		}
		out.Pruned = pruned
		// In full mode, a release pruned right after its download wasn't archived after all.
		if !pol.allNotes {
			out.New = slices.DeleteFunc(out.New, func(t string) bool { return slices.Contains(pruned, t) })
		}
	}
	return out
}

// saveRelease writes a release's notes and metadata to dir and, with files, downloads its
// assets (skipping those over maxAsset bytes).
func (a *Archiver) saveRelease(ctx context.Context, rel forge.Release, dir, label string, files bool, maxAsset int64) (complete bool, skipped []string, err error) {
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
	if !files {
		return true, nil, nil
	}
	complete = true
	var firstErr error
	for i, asset := range rel.Assets {
		live.From(ctx).SetStep("Downloading release", fmt.Sprintf("%s · asset %d/%d: %s", label, i+1, len(rel.Assets), asset.Name))
		_, err := a.forge.Download(ctx, asset, filepath.Join(dir, "assets"), maxAsset)
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

// DueRepos picks the repositories a check cycle should check, in order: every full
// repository, then light ones whose own check interval has passed (all of them with force),
// then light ones not cloned yet, at most light.max_clones_per_cycle of those.
func (a *Archiver) DueRepos(repos []*store.Repo, now time.Time, force bool) []int64 {
	var full, light, clones []*store.Repo
	// A light repo is due slightly early so it isn't pushed back by a whole cycle each time.
	interval := a.cfg.Light.CheckInterval - a.cfg.CheckInterval/2
	for _, r := range repos {
		switch {
		case r.Mode != store.ModeLight:
			full = append(full, r)
		case !r.Cloned && r.FailCount == 0:
			clones = append(clones, r)
		case force || now.Sub(r.LastChecked) >= interval:
			light = append(light, r)
		}
	}
	if max := a.cfg.Light.MaxClonesPerCycle; max > 0 && len(clones) > max {
		clones = clones[:max]
	}
	var ids []int64
	for _, group := range [][]*store.Repo{full, light, clones} {
		for _, r := range group {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// RunCycle checks the repositories that are due (see DueRepos) with bounded concurrency.
// With force, light repositories are checked whatever their interval. Returns false if a
// cycle was already running.
func (a *Archiver) RunCycle(ctx context.Context, force bool) (results []Result, ran bool) {
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
	ids := a.DueRepos(repos, start, force)
	a.log.Info().Int("repos", len(ids)).Int("tracked", len(repos)).Msg("Starting check cycle")
	a.Live.CycleStart(len(ids))
	what := fmt.Sprintf("%d repositories", len(ids))
	if len(ids) < len(repos) {
		what = fmt.Sprintf("%d of %d repositories; the other light ones aren't due yet", len(ids), len(repos))
	}
	a.Live.Event(0, "", "info", "Check cycle started ("+what+")")
	results = a.checkMany(ctx, ids, true, "")
	took := time.Since(start)
	a.Live.CycleEnd()
	changed, failed := 0, 0
	for _, r := range results {
		if r.Err != nil {
			failed++
		} else if r.Changed() {
			changed++
		}
	}
	a.Live.Event(0, "", "info", fmt.Sprintf("Check cycle finished in %s: %d changed, %d failed", took.Round(time.Second), changed, failed))
	a.lastCycleEnd.Store(time.Now().Unix())
	a.lastCycleDur.Store(took.Milliseconds())
	a.log.Info().Int("repos", len(results)).Dur("took", took).Msg("Check cycle finished")
	a.CheckDisk(ctx)
	return results, true
}

// CheckMany checks the given repositories with bounded concurrency.
func (a *Archiver) CheckMany(ctx context.Context, ids []int64) (results []Result) {
	return a.checkMany(ctx, ids, false, "")
}

// checkMany runs the checks; cycle checks report on the hub's worker slots and progress.
// Other checks show on the live page under label (default "Manual check").
func (a *Archiver) checkMany(ctx context.Context, ids []int64, cycle bool, label string) (results []Result) {
	jobs := make(chan int64)
	out := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < a.cfg.Concurrency; i++ {
		wg.Add(1)
		wctx := ctx
		if cycle {
			wctx = live.WithSlot(ctx, a.Live.Worker(i))
		}
		go func() {
			defer wg.Done()
			ctx := wctx
			for id := range jobs {
				// A repo still busy with an earlier check (e.g. hours of release downloads
				// after `add`) is skipped instead of holding up the whole cycle.
				m := a.lock(id)
				if !m.TryLock() {
					a.log.Debug().Int64("repo_id", id).Msg("Skipping repository that is still being checked")
					if cycle {
						a.Live.CycleTick()
					}
					continue
				}
				jctx := ctx
				if !cycle && label != "" {
					jctx = live.WithSlot(ctx, a.Live.Temp(label))
				}
				res := a.check(jctx, id)
				m.Unlock()
				if cycle {
					a.Live.CycleTick()
				}
				out <- res
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
	if a.mirror != nil {
		go a.runMirror(ctx)
	}

	// Give the bot a moment to connect before the first cycle.
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}
	// Sync the sources first, so what they add is cloned in the first cycle.
	for _, src := range a.cfg.Sources {
		a.syncAndReport(ctx, src)
		go a.runSource(ctx, src)
	}
	ticker := time.NewTicker(a.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		a.Live.SetNext(time.Now().Add(a.cfg.CheckInterval))
		a.RunCycle(ctx, false)
		a.KickMirror()
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
	mst, _, _ := a.store.MirrorState(ctx, repo.ID)
	if err := a.store.RemoveRepo(ctx, repo.ID); err != nil {
		return err
	}
	a.unpublishMirror(ctx, repo, mst.Path, purge)
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
