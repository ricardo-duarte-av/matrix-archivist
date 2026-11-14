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
	Size      int64          // bytes on disk if archived, else the estimated download size
	Release   *forge.Release // nil if the release no longer exists upstream
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
func (a *Archiver) releaseCandidates(ctx context.Context, repo *store.Repo, rels []forge.Release) ([]ReleaseCandidate, error) {
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
		if _, ok := have[rel.Tag]; ok {
			c.Archived = true
			c.Size = gitops.DirSize(a.releaseDir(repo, rel.Tag))
		} else {
			c.Size = a.estimateSize(rel)
		}
		out = append(out, c)
	}
	for _, r := range archived {
		if !seen[r.Tag] {
			out = append(out, ReleaseCandidate{Tag: r.Tag, Published: r.Published, Archived: true, Size: gitops.DirSize(a.releaseDir(repo, r.Tag))})
		}
	}
	return out, nil
}

func (a *Archiver) releaseDir(repo *store.Repo, tag string) string {
	return filepath.Join(a.releasesDir(repo), forge.SafeName(tag))
}

// estimateSize is the size of the assets that would be downloaded (as reported by the forge).
func (a *Archiver) estimateSize(rel *forge.Release) int64 {
	if !a.cfg.Releases.DownloadAssets {
		return 0
	}
	var n int64
	for _, as := range rel.Assets {
		if max := a.cfg.Releases.MaxAssetSize; max > 0 && as.Size > max {
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

func (a *Archiver) planReleases(repo *store.Repo, cands []ReleaseCandidate) []ReleaseCandidate {
	r := a.cfg.Releases
	if !r.Limited() {
		return cands
	}
	return PlanReleases(cands, r.KeepLast, r.MaxRepoSize, a.baseSize(repo))
}

// pruneReleases deletes archived releases (and leftover partial downloads) that the
// retention plan no longer keeps. It returns the tags removed.
func (a *Archiver) pruneReleases(ctx context.Context, repo *store.Repo, rels []forge.Release, log zerolog.Logger) ([]string, error) {
	cands, err := a.releaseCandidates(ctx, repo, rels)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, c := range a.planReleases(repo, cands) {
		keep[forge.SafeName(c.Tag)] = true
	}
	var pruned []string
	for _, c := range cands {
		if keep[forge.SafeName(c.Tag)] || !c.Archived {
			continue
		}
		if err := os.RemoveAll(a.releaseDir(repo, c.Tag)); err != nil {
			return pruned, err
		}
		if err := a.store.RemoveRelease(ctx, repo.ID, c.Tag); err != nil {
			return pruned, err
		}
		log.Info().Str("tag", c.Tag).Int64("size", c.Size).Msg("Removed release (retention limits)")
		pruned = append(pruned, c.Tag)
	}
	// Directories of releases outside the plan that never completed (e.g. a download
	// interrupted before the limits were set).
	entries, _ := os.ReadDir(a.releasesDir(repo))
	for _, e := range entries {
		if e.IsDir() && !keep[e.Name()] {
			_ = os.RemoveAll(filepath.Join(a.releasesDir(repo), e.Name()))
		}
	}
	return pruned, nil
}
