package archiver

import (
	"context"
	"time"

	"github.com/rs/zerolog"

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
	if (res.Changed() || repo.ReadmeAt.IsZero()) && dirExists(a.checkoutDir(repo)) {
		info, _ := readme.Read(a.checkoutDir(repo))
		if err := a.store.SetReadme(ctx, repo.ID, store.Readme{
			Title: info.Title, Summary: info.Summary, Links: info.Links, Text: info.Text,
		}); err != nil {
			log.Err(err).Msg("Failed to save README metadata")
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
	ictx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	info, err := a.forge.Info(ictx, p, res.url.Owner, res.url.Name)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to fetch repository description")
	}
	_ = a.store.SetForgeInfo(ctx, repo.ID, info.Description, info.Homepage, info.Topics, err == nil)
}
