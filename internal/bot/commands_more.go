package bot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/archiver"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// FilterRepos keeps repos matching every term: "#tag" matches a tag exactly, anything else is
// a case-insensitive substring of the URL, note or tags.
func FilterRepos(repos []*store.Repo, terms []string) []*store.Repo {
	var out []*store.Repo
	for _, r := range repos {
		hay := strings.ToLower(r.URL + "\n" + r.Notes + "\n" + strings.Join(r.Tags, " "))
		match := true
		for _, t := range terms {
			if tag, ok := strings.CutPrefix(t, "#"); ok && tag != "" && !isDigits(tag) {
				if !slices.Contains(r.Tags, store.NormalizeTag(tag)) {
					match = false
					break
				}
				continue
			}
			if !strings.Contains(hay, strings.ToLower(t)) {
				match = false
				break
			}
		}
		if match {
			out = append(out, r)
		}
	}
	return out
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// searchLimit caps how many search results are shown.
const searchLimit = 50

func (b *Bot) cmdSearch(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s search <text|#tag>…`", b.cfg.CommandPrefix))
		return
	}
	found, err := b.store.SearchRepos(b.ctx, args)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	if len(found) == 0 {
		b.reply(evt, "No repositories match.")
		return
	}
	found = RankRepos(found, args)
	b.arch.Annotate(found...)
	intro := fmt.Sprintf("**%d matching repositories**", len(found))
	outro := ""
	if len(found) > searchLimit {
		intro = fmt.Sprintf("**%d matching repositories** (best %d shown)", len(found), searchLimit)
		outro = fmt.Sprintf("_…and %d more; add more words to narrow the search._", len(found)-searchLimit)
		found = found[:searchLimit]
	}
	rows := make([][]cell, len(found))
	for i, r := range found {
		rows[i] = repoRow(r)
		if r.Notes != "" {
			rows[i][2] = joinCells(" — ", rows[i][2], cell{"<em>" + html.EscapeString(oneLineMD(r.Notes, 120)) + "</em>", oneLineMD(r.Notes, 120)})
		}
	}
	b.replyRich(evt, TableMessages(intro, listHead, rows, outro)...)
}

func (b *Bot) cmdInfo(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s info <#id|url>`", b.cfg.CommandPrefix))
		return
	}
	r, err := b.resolve(args[0])
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	b.arch.Annotate(r)
	d := b.arch.Details(b.ctx, r, 5)
	nrel, _ := b.store.ReleaseCount(b.ctx, r.ID)
	latest, published, _ := b.store.LatestRelease(b.ctx, r.ID)

	m := &rich{}
	head := fmt.Sprintf("**#%d** %s", r.ID, r.URL)
	if about := About(r); about != "" {
		head += "  \n" + oneLineMD(about, 300)
	}
	m.MD(head)
	if r.Notes != "" {
		m.MD("> " + strings.ReplaceAll(r.Notes, "\n", "\n> "))
	}

	kv := func(k string, v cell) []cell { return []cell{cell{"<strong>" + k + "</strong>", k}, v} }
	rows := [][]cell{kv("State", textCell(repoState(r)))}
	if r.Upstream != "" {
		rows = append(rows, kv("Upstream", textCell(fmt.Sprintf("%s since %s", r.UpstreamDetail, r.UpstreamSince.Format("2006-01-02 15:04 MST")))))
	}
	if links := linksCell(r); links.text != "" {
		rows = append(rows, kv("Links", links))
	}
	if len(r.Tags) > 0 {
		rows = append(rows, kv("Tags", mdCell("`"+strings.Join(r.Tags, "` `")+"`")))
	}
	if len(r.Topics) > 0 {
		rows = append(rows, kv("Topics", textCell(strings.Join(r.Topics, ", "))))
	}
	rows = append(rows,
		kv("Added", textCell(fmt.Sprintf("by %s, %s", r.AddedBy, ago(r.AddedAt)))),
		kv("Checked", textCell(fmt.Sprintf("%s · last changed %s", ago(r.LastChecked), ago(r.LastChanged)))))
	if d.Branch != "" {
		rows = append(rows, kv("Default branch", mdCell("`"+d.Branch+"`")))
	}
	if nrel > 0 {
		rows = append(rows, kv("Releases", textCell(fmt.Sprintf("%d archived (latest: %s, %s)", nrel, latest, published.Format("2006-01-02")))))
	} else {
		rows = append(rows, kv("Releases", textCell("none archived")))
	}
	if n := len(d.Snapshots); n > 0 {
		refs := 0
		for _, s := range d.Snapshots {
			refs += s.Refs
		}
		rows = append(rows, kv("History snapshots", mdCell(fmt.Sprintf("%d (%d refs preserved from rewrites/deletions, latest `refs/archive/%s`)", n, refs, d.Snapshots[n-1].Stamp))))
	}
	rows = append(rows, kv("Disk usage", mdCell(fmt.Sprintf("%s (`%s`)", gitops.HumanSize(gitops.DirSize(b.arch.RepoDir(r))), r.RelDir))))
	if u := b.cfg.HTTP.BrowseURL(r.RelDir); u != "" {
		rows = append(rows, kv("Browse", linkCell(u, u)))
	}
	if r.LastError != "" {
		rows = append(rows, kv("Last error", cell{"<code>" + html.EscapeString(oneLineMD(r.LastError, 300)) + "</code>", oneLineMD(r.LastError, 300)}))
	}
	m.Table(nil, rows)

	if len(d.Commits) > 0 {
		m.MD("**Recent commits**")
		var cs [][]cell
		for _, c := range d.Commits {
			cs = append(cs, []cell{
				{"<code>" + html.EscapeString(c.Hash) + "</code>", c.Hash},
				textCell(oneLineMD(c.Subject, 100)),
				textCell(oneLineMD(c.Author, 40)),
				textCell(ago(c.Time)),
			})
		}
		m.Table([]string{"Commit", "Subject", "Author", "When"}, cs)
	}
	b.replyRich(evt, m)
}

func (b *Bot) cmdNote(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s note <#id> [text|--clear]`", b.cfg.CommandPrefix))
		return
	}
	r, err := b.resolve(args[0])
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	switch {
	case len(args) == 1:
		if r.Notes == "" {
			b.reply(evt, fmt.Sprintf("**#%d** has no note.", r.ID))
		} else {
			b.reply(evt, fmt.Sprintf("**#%d** note:\n\n> %s", r.ID, strings.ReplaceAll(r.Notes, "\n", "\n> ")))
		}
		return
	case len(args) == 2 && (args[1] == "--clear" || args[1] == "-"):
		err = b.store.SetNotes(b.ctx, r.ID, "")
	default:
		// Take the note from the original body to keep its line breaks and spacing.
		err = b.store.SetNotes(b.ctx, r.ID, noteText(evt, args))
	}
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	b.react(evt, "✅")
}

// noteText returns everything after "<prefix> note <#id>" in the message body.
func noteText(evt *event.Event, args []string) string {
	fallback := strings.Join(args[1:], " ")
	msg := evt.Content.AsMessage()
	if msg == nil {
		return fallback
	}
	body := stripReplyFallback(msg.Body)
	// Skip prefix, command and target.
	rest := body
	for range 3 {
		rest = strings.TrimLeft(rest, " \t\n")
		i := strings.IndexAny(rest, " \t\n")
		if i < 0 {
			return fallback
		}
		rest = rest[i:]
	}
	if t := strings.TrimSpace(rest); t != "" {
		if len(t) > 2000 {
			t = t[:2000]
		}
		return t
	}
	return fallback
}

func (b *Bot) cmdTag(evt *event.Event, args []string) { b.changeTags(evt, args, true) }

func (b *Bot) cmdUntag(evt *event.Event, args []string) { b.changeTags(evt, args, false) }

func (b *Bot) changeTags(evt *event.Event, args []string, add bool) {
	if len(args) < 2 {
		verb := "tag"
		if !add {
			verb = "untag"
		}
		b.reply(evt, fmt.Sprintf("Usage: `%s %s <#id> <tag>…`", b.cfg.CommandPrefix, verb))
		return
	}
	r, err := b.resolve(args[0])
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	tags := r.Tags
	for _, t := range args[1:] {
		t = store.NormalizeTag(t)
		if add {
			tags = append(tags, t)
		} else {
			tags = slices.DeleteFunc(tags, func(x string) bool { return x == t })
		}
	}
	if err := b.store.SetTags(b.ctx, r.ID, tags); err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	r, _ = b.store.Repo(b.ctx, r.ID)
	if len(r.Tags) == 0 {
		b.reply(evt, fmt.Sprintf("**#%d** has no tags.", r.ID))
	} else {
		b.reply(evt, fmt.Sprintf("**#%d** tags: `%s`", r.ID, strings.Join(r.Tags, "`, `")))
	}
}

func (b *Bot) cmdUsage(evt *event.Event, _ []string) {
	repos, err := b.store.Repos(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].SizeBytes > repos[j].SizeBytes })
	var total int64
	for _, r := range repos {
		total += r.SizeBytes
	}
	intro := fmt.Sprintf("**Disk usage:** %s in %d repositories%s", gitops.HumanSize(total), len(repos), freeSpaceText(b.cfg.StorageDir))
	const max = 25
	outro := "_Sizes are measured after each change._"
	if len(repos) > max {
		outro = fmt.Sprintf("_…and %d smaller ones. Sizes are measured after each change._", len(repos)-max)
		repos = repos[:max]
	}
	rows := make([][]cell, len(repos))
	for i, r := range repos {
		pct := 0.0
		if total > 0 {
			pct = float64(r.SizeBytes) * 100 / float64(total)
		}
		rows[i] = []cell{idCell(r), linkCell(r.URL, displayURL(r.URL)), textCell(gitops.HumanSize(r.SizeBytes)), textCell(fmt.Sprintf("%.0f%%", pct))}
	}
	b.replyRich(evt, TableMessages(intro, []string{"#", "Repository", "Size", "Share"}, rows, outro)...)
}

// freeSpaceText is ", 120.3 GiB free of 500.0 GiB" or "" if unknown.
func freeSpaceText(dir string) string {
	free, total, err := archiver.FreeSpace(dir)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" · %s free of %s", gitops.HumanSize(int64(free)), gitops.HumanSize(int64(total)))
}

// ExportFile is the JSON format written by export and read by import.
type ExportFile struct {
	Format     string        `json:"format"`
	ExportedAt time.Time     `json:"exported_at"`
	Repos      []ExportEntry `json:"repos"`
}

type ExportEntry struct {
	URL     string    `json:"url"`
	Notes   string    `json:"notes,omitempty"`
	Tags    []string  `json:"tags,omitempty"`
	AddedBy string    `json:"added_by,omitempty"`
	AddedAt time.Time `json:"added_at,omitzero"`
}

const exportFormat = "matrix-archivist/v1"

// Export serialises the repository list.
func Export(repos []*store.Repo, now time.Time) []byte {
	f := ExportFile{Format: exportFormat, ExportedAt: now.UTC(), Repos: []ExportEntry{}}
	for _, r := range repos {
		f.Repos = append(f.Repos, ExportEntry{URL: r.URL, Notes: r.Notes, Tags: r.Tags, AddedBy: r.AddedBy, AddedAt: r.AddedAt.UTC()})
	}
	out, _ := json.MarshalIndent(f, "", "  ")
	return append(out, '\n')
}

// ParseImport reads an export file, or plain text with one URL per line ("#" starts a
// comment line; anything after the first whitespace is ignored).
func ParseImport(data []byte) ([]ExportEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		var f ExportFile
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		if f.Format != exportFormat {
			return nil, fmt.Errorf("unknown export format %q", f.Format)
		}
		return f.Repos, nil
	}
	var out []ExportEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		out = append(out, ExportEntry{URL: strings.Fields(line)[0]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no repository URLs found in the file")
	}
	return out, nil
}

func (b *Bot) cmdExport(evt *event.Event, _ []string) {
	repos, err := b.store.Repos(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	now := time.Now()
	name := fmt.Sprintf("archivist-export-%s.json", now.Format("2006-01-02"))
	if err := b.sendFile(evt, name, "application/json", Export(repos, now)); err != nil {
		b.reply(evt, "❌ Upload failed: "+err.Error())
	}
}

func (b *Bot) cmdImport(evt *event.Event, _ []string) {
	msg := evt.Content.AsMessage()
	target := msg.RelatesTo.GetNonFallbackReplyTo()
	if target == "" {
		b.reply(evt, fmt.Sprintf("Reply to an exported JSON file (or a text file with one URL per line) with `%s import`.", b.cfg.CommandPrefix))
		return
	}
	done := b.progress(evt)
	name, data, err := b.fetchFile(b.ctx, target)
	if err != nil {
		done(false)
		b.reply(evt, "❌ "+err.Error())
		return
	}
	entries, err := ParseImport(data)
	if err != nil {
		done(false)
		b.reply(evt, fmt.Sprintf("❌ `%s`: %v", name, err))
		return
	}
	var added []*store.Repo
	var existing, invalid int
	var problems []string
	for _, e := range entries {
		u, err := gitops.ParseRepoURL(e.URL)
		if err != nil {
			invalid++
			if len(problems) < 10 {
				problems = append(problems, fmt.Sprintf("- ❌ `%s`: %v", strings.ReplaceAll(e.URL, "`", "'"), err))
			}
			continue
		}
		repo, err := b.store.AddRepo(b.ctx, u.Raw, u.Key(), u.RelDir(), evt.Sender.String())
		switch {
		case errors.Is(err, store.ErrDuplicate):
			existing++
			// Merge metadata into the existing entry without overwriting a local note.
			if len(e.Tags) > 0 {
				_ = b.store.SetTags(b.ctx, repo.ID, append(repo.Tags, e.Tags...))
			}
			if repo.Notes == "" && e.Notes != "" {
				_ = b.store.SetNotes(b.ctx, repo.ID, e.Notes)
			}
			continue
		case err != nil:
			invalid++
			problems = append(problems, fmt.Sprintf("- ❌ `%s`: %v", u.Raw, err))
			continue
		}
		if e.Notes != "" {
			_ = b.store.SetNotes(b.ctx, repo.ID, e.Notes)
		}
		if len(e.Tags) > 0 {
			_ = b.store.SetTags(b.ctx, repo.ID, e.Tags)
		}
		added = append(added, repo)
	}
	text := fmt.Sprintf("📥 Imported `%s`: **%d new**, %d already tracked, %d invalid.", name, len(added), existing, invalid)
	if len(problems) > 0 {
		text += "\n\n" + strings.Join(problems, "\n")
	}
	if len(added) > 0 {
		text += "\n\nCloning the new repositories in the background…"
	}
	b.reply(evt, text)
	if len(added) == 0 {
		done(invalid == 0)
		return
	}
	ids := make([]int64, len(added))
	for i, r := range added {
		ids[i] = r.ID
	}
	results := b.arch.CheckMany(b.ctx, ids)
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
		}
	}
	done(failed == 0 && invalid == 0)
	b.replyRich(evt, cycleReport(results)...)
}

// oneLineMD flattens text to a single line, trimmed to max runes.
func oneLineMD(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
