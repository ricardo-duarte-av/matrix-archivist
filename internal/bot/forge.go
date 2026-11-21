package bot

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/archiver"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// mirrorCounts is a one-line forge sync overview: "650 of 700 published, 48 waiting, 2 failing".
func mirrorCounts(o archiver.MirrorOverview) string {
	s := fmt.Sprintf("%d of %d archived repositories published", o.Synced, o.Archived)
	if o.Pending > 0 {
		s += fmt.Sprintf(", %d waiting", o.Pending)
	}
	if o.Failing > 0 {
		s += fmt.Sprintf(", %d failing", o.Failing)
	}
	if o.Warnings > 0 {
		s += fmt.Sprintf(", %d with warnings", o.Warnings)
	}
	return s
}

// mirrorCell describes a repository's copy on the forge for `info`.
func (b *Bot) mirrorCell(r *store.Repo) cell {
	st, ok, err := b.store.MirrorState(b.ctx, r.ID)
	switch {
	case err != nil:
		return textCell(err.Error())
	case !ok || st.Path == "":
		return textCell("not published yet")
	}
	link := b.cfg.Mirror.RepoURL(st.Path)
	md := fmt.Sprintf("[%s](%s)", st.Path, link)
	switch {
	case st.FailCount > 0:
		md += fmt.Sprintf(" · ❌ %d failed attempts, last %s: `%s`", st.FailCount, ago(st.TriedAt), oneLineMD(st.Error, 200))
	case st.SyncedAt.IsZero():
		md += " · not published yet"
	default:
		md += " · synced " + ago(st.SyncedAt)
		if st.Error != "" {
			md += fmt.Sprintf(" · ⚠️ `%s`", oneLineMD(st.Error, 200))
		}
	}
	return mdCell(md)
}

func (b *Bot) cmdForge(evt *event.Event, args []string) {
	if !b.arch.MirrorEnabled() {
		b.reply(evt, "The forge mirror is not enabled (see `mirror:` in the config).")
		return
	}
	if len(args) == 0 || strings.EqualFold(args[0], "status") {
		b.forgeStatus(evt)
		return
	}
	if !strings.EqualFold(args[0], "sync") || len(args) < 2 {
		b.reply(evt, fmt.Sprintf("Usage: `%s forge [sync <#id|url|all>]`", b.cfg.CommandPrefix))
		return
	}
	if lvl := b.powerLevel(b.ctx, evt.Sender); lvl < b.cfg.MinPowerLevel {
		b.reply(evt, fmt.Sprintf("⛔ You need power level %d for `forge sync` (you have %d).", b.cfg.MinPowerLevel, lvl))
		return
	}
	if strings.EqualFold(args[1], "all") {
		if err := b.store.ResetMirror(b.ctx, 0); err != nil {
			b.reply(evt, "❌ "+err.Error())
			return
		}
		b.arch.KickMirror()
		b.reply(evt, "🪞 Every repository will be published to the forge again in the next pass (it runs in the background; `forge` shows the progress).")
		return
	}
	var repos []*store.Repo
	var lines []string
	for _, a := range args[1:] {
		r, err := b.resolve(a)
		if err != nil {
			lines = append(lines, "- ❌ "+err.Error())
			continue
		}
		repos = append(repos, r)
	}
	if len(repos) > 0 {
		done := b.progress(evt)
		ok := true
		for _, r := range repos {
			res := b.arch.SyncMirror(b.ctx, r.ID, true)
			ok = ok && res.Err == nil
			line := fmt.Sprintf("- **#%d** %s: %s", r.ID, r.URL, res.Text())
			if res.Path != "" && res.Err == nil {
				line += fmt.Sprintf(" → %s", b.cfg.Mirror.RepoURL(res.Path))
			}
			lines = append(lines, line)
		}
		done(ok)
	}
	b.reply(evt, strings.Join(lines, "\n"))
}

func (b *Bot) forgeStatus(evt *event.Event) {
	o, err := b.arch.MirrorOverview(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	m := &rich{}
	m.MD(fmt.Sprintf("**Forge mirror** [%s](%s): %s.", b.cfg.Mirror.PublicURL, b.cfg.Mirror.PublicURL, mirrorCounts(o)))
	if o.Pending > 0 && b.cfg.Mirror.MaxNewPerCycle > 0 {
		m.MD(fmt.Sprintf("New repositories are published %d per pass (after each check cycle, every %s).", b.cfg.Mirror.MaxNewPerCycle, b.cfg.CheckInterval))
	}
	if len(o.Failures) > 0 {
		var rows [][]cell
		for i, f := range o.Failures {
			if i == 20 {
				break
			}
			rows = append(rows, []cell{idCell(f.Repo), linkCell(f.Repo.URL, displayURL(f.Repo.URL)),
				textCell(fmt.Sprintf("%d× · %s", f.State.FailCount, ago(f.State.TriedAt))), textCell(oneLineMD(f.State.Error, 200))})
		}
		m.MD("**Failing**")
		m.Table([]string{"#", "Repository", "Attempts", "Last error"}, rows)
	}
	b.replyRich(evt, m)
}
