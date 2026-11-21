package archiver

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/forge"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// ReleaseCandidate is a release that may be kept: listed upstream, archived here, or both.
type ReleaseCandidate struct {
	Tag       string
	Published time.Time
	Archived  bool
	MetaOnly  bool // archived without its files (notes and metadata only)
	// Incomplete is archived with its files, but some of them are missing from disk (deleted
	// by hand or by a mistake); they are downloaded again.
	Incomplete bool
	Size       int64          // bytes on disk if archived with files, else the estimated download size
	Release    *forge.Release // nil if the release no longer exists upstream
}

// releasePolicy is how a repository's releases are archived, depending on its mode.
type releasePolicy struct {
	downloadAssets bool
	maxAssetSize   int64
	keepLast       int
	maxRepoSize    int64
	// allNotes keeps the notes and metadata of every release, including those outside the
	// limits, which only get no files (light mode). Otherwise releases outside the limits
	// aren't archived at all.
	allNotes bool
}

func (p releasePolicy) limited() bool { return p.keepLast > 0 || p.maxRepoSize > 0 }

func (a *Archiver) releasePolicy(repo *store.Repo) releasePolicy {
	if repo.Mode == store.ModeLight {
		l := a.cfg.Light.Releases
		return releasePolicy{l.DownloadAssets, l.MaxAssetSize, l.KeepLast, l.MaxRepoSize, true}
	}
	r := a.cfg.Releases
	return releasePolicy{r.DownloadAssets, r.MaxAssetSize, r.KeepLast, r.MaxRepoSize, false}
}

// PlanReleases picks the releases to keep, newest first: at most keepLast (0 = no limit),
// and only as many as fit in maxRepo bytes (0 = no limit) on top of base (the repository's
// size without releases). The newest release is always kept.
func PlanReleases(cands []ReleaseCandidate, keepLast int, maxRepo, base int64) []ReleaseCandidate {
	sorted := append([]ReleaseCandidate(nil), cands...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Published.Equal(sorted[j].Published) {
			return sorted[i].Published.After(sorted[j].Published)
		}
		return sorted[i].Tag > sorted[j].Tag
	})
	if keepLast > 0 && len(sorted) > keepLast {
		sorted = sorted[:keepLast]
	}
	if maxRepo <= 0 {
		return sorted
	}
	total := base
	for i, c := range sorted {
		total += c.Size
		if i > 0 && total > maxRepo {
			return sorted[:i]
		}
	}
	return sorted
}

// releaseCandidates merges the upstream listing with releases archived earlier (which may
// have been deleted upstream since).
func (a *Archiver) releaseCandidates(ctx context.Context, repo *store.Repo, pol releasePolicy, rels []forge.Release) ([]ReleaseCandidate, error) {
	archived, err := a.store.ArchivedReleases(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	have := map[string]store.ArchivedRelease{}
	for _, r := range archived {
		have[r.Tag] = r
	}
	var out []ReleaseCandidate
	seen := map[string]bool{}
	for i := range rels {
		rel := &rels[i]
		if rel.Draft || rel.Tag == "" || (rel.Prerelease && !a.cfg.Releases.IncludePrereleases) || seen[rel.Tag] {
			continue
		}
		seen[rel.Tag] = true
		c := ReleaseCandidate{Tag: rel.Tag, Published: rel.Published, Release: rel}
		if ar, ok := have[rel.Tag]; ok && !ar.MetaOnly {
			c.Archived = true
			c.Size = gitops.DirSize(a.releaseDir(repo, rel.Tag))
			c.Incomplete = missingAssets(pol, rel, filepath.Join(a.releaseDir(repo, rel.Tag), "assets"))
		} else {
			c.Archived, c.MetaOnly = ok, ok
			c.Size = estimateSize(pol, rel)
		}
		out = append(out, c)
	}
	for _, r := range archived {
		if !seen[r.Tag] {
			c := ReleaseCandidate{Tag: r.Tag, Published: r.Published, Archived: true, MetaOnly: r.MetaOnly}
			if !r.MetaOnly {
				c.Size = gitops.DirSize(a.releaseDir(repo, r.Tag))
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *Archiver) releaseDir(repo *store.Repo, tag string) string {
	return filepath.Join(a.releasesDir(repo), forge.SafeName(tag))
}

// missingAssets reports whether a release archived with its files lacks some of them on disk
// (files over the size limit are skipped on purpose and don't count).
func missingAssets(pol releasePolicy, rel *forge.Release, dir string) bool {
	if !pol.downloadAssets {
		return false
	}
	for _, as := range rel.Assets {
		if max := pol.maxAssetSize; max > 0 && as.Size > max {
			continue
		}
		name := forge.SafeName(as.Name)
		if name == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return true
		}
	}
	return false
}

// estimateSize is the size of the assets that would be downloaded (as reported by the forge).
func estimateSize(pol releasePolicy, rel *forge.Release) int64 {
	if !pol.downloadAssets {
		return 0
	}
	var n int64
	for _, as := range rel.Assets {
		if max := pol.maxAssetSize; max > 0 && as.Size > max {
			continue
		}
		n += as.Size
	}
	return n
}

// baseSize is the repository's size without its releases.
func (a *Archiver) baseSize(repo *store.Repo) int64 {
	return gitops.DirSize(a.RepoDir(repo)) - gitops.DirSize(a.releasesDir(repo))
}

// planReleases returns the releases that get their files. With download_assets off in light
// mode, none do.
func (a *Archiver) planReleases(repo *store.Repo, pol releasePolicy, cands []ReleaseCandidate) []ReleaseCandidate {
	if pol.allNotes && !pol.downloadAssets {
		return nil
	}
	if !pol.limited() {
		return cands
	}
	return PlanReleases(cands, pol.keepLast, pol.maxRepoSize, a.baseSize(repo))
}

// pruneReleases removes archived releases (and leftover partial downloads) that the
// retention plan no longer keeps. In light mode only their files are removed and their notes
// stay. Releases already reduced to notes are never removed. It returns the tags pruned.
func (a *Archiver) pruneReleases(ctx context.Context, repo *store.Repo, pol releasePolicy, rels []forge.Release, log zerolog.Logger) ([]string, error) {
	cands, err := a.releaseCandidates(ctx, repo, pol, rels)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, c := range a.planReleases(repo, pol, cands) {
		keep[forge.SafeName(c.Tag)] = true
	}
	notesOnly := map[string]bool{}
	var pruned []string
	for _, c := range cands {
		name := forge.SafeName(c.Tag)
		switch {
		case keep[name]:
			continue
		case c.MetaOnly:
			notesOnly[name] = true
			continue
		case !c.Archived:
			continue
		case pol.allNotes:
			if err := os.RemoveAll(filepath.Join(a.releaseDir(repo, c.Tag), "assets")); err != nil {
				return pruned, err
			}
			if err := a.store.SetReleaseMetaOnly(ctx, repo.ID, c.Tag); err != nil {
				return pruned, err
			}
			notesOnly[name] = true
			log.Info().Str("tag", c.Tag).Int64("size", c.Size).Msg("Removed release files (retention limits)")
		default:
			if err := os.RemoveAll(a.releaseDir(repo, c.Tag)); err != nil {
				return pruned, err
			}
			if err := a.store.RemoveRelease(ctx, repo.ID, c.Tag); err != nil {
				return pruned, err
			}
			log.Info().Str("tag", c.Tag).Int64("size", c.Size).Msg("Removed release (retention limits)")
		}
		pruned = append(pruned, c.Tag)
	}
	// Leftovers of releases outside the plan that never completed (e.g. a download
	// interrupted before the limits were set): whole directories, or the files of releases
	// kept as notes only.
	entries, _ := os.ReadDir(a.releasesDir(repo))
	for _, e := range entries {
		dir := filepath.Join(a.releasesDir(repo), e.Name())
		switch {
		case !e.IsDir() || keep[e.Name()]:
		case notesOnly[e.Name()]:
			_ = os.RemoveAll(filepath.Join(dir, "assets"))
		default:
			_ = os.RemoveAll(dir)
		}
	}
	return pruned, nil
}
