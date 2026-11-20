package archiver

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/live"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/readme"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// forgeInfoMaxAge is how often the forge is asked for a repo's description/homepage/topics.
const forgeInfoMaxAge = 24 * time.Hour

// updateMeta refreshes README-derived metadata when the checkout changed (or was never read)
// and, when askForge is set, the forge description once a day. Failures are logged and
// otherwise ignored.
func (a *Archiver) updateMeta(ctx context.Context, res Result, askForge bool, log zerolog.Logger) {
	repo := res.Repo
	if res.Changed() || repo.ReadmeAt.IsZero() {
		var info readme.Info
		ok := false
		if dirExists(a.checkoutDir(repo)) {
			live.From(ctx).SetStep("Reading README", "")
			info, _ = readme.Read(a.checkoutDir(repo))
			ok = true
		} else if dirExists(a.mirrorDir(repo)) {
			// Light archives have no checkout: read the README from the mirror's HEAD.
			live.From(ctx).SetStep("Reading README", "")
			info, ok = a.mirrorReadme(ctx, repo)
		}
		if ok {
			if err := a.store.SetReadme(ctx, repo.ID, store.Readme{
				Title: info.Title, Summary: info.Summary, Links: info.Links, Text: info.Text,
			}); err != nil {
				log.Err(err).Msg("Failed to save README metadata")
			}
		}
	}
	if !askForge || !res.urlOK || time.Since(repo.ForgeInfoAt) < forgeInfoMaxAge || ctx.Err() != nil {
		return
	}
	p := a.provider(ctx, res.url.Host)
	if p == nil {
		_ = a.store.SetForgeInfo(ctx, repo.ID, "", "", nil, false)
		return
	}
	live.From(ctx).SetStep("Fetching description", res.url.Host)
	ictx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	info, err := a.forge.Info(ictx, p, res.url.Owner, res.url.Name)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to fetch repository description")
	}
	_ = a.store.SetForgeInfo(ctx, repo.ID, info.Description, info.Homepage, info.Topics, err == nil)
}

// mirrorReadme extracts the README at the top of the mirror's default branch. ok is false
// if the mirror can't be read (an empty repository without a README is ok).
func (a *Archiver) mirrorReadme(ctx context.Context, repo *store.Repo) (readme.Info, bool) {
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	mirror := a.mirrorDir(repo)
	files, err := a.git.RootFiles(rctx, mirror)
	if err != nil {
		return readme.Info{}, false // e.g. an empty repository; tried again after a change
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	name := readme.Pick(names)
	if name == "" {
		return readme.Info{}, true
	}
	data, err := a.git.ReadBlob(rctx, mirror, files[name], readme.MaxText)
	if err != nil {
		return readme.Info{}, false
	}
	return readme.Extract(string(data)), true
}
