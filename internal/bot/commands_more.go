package bot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

func (b *Bot) cmdSearch(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s search <text|#tag>…`", b.cfg.CommandPrefix))
		return
	}
	repos, err := b.store.Repos(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	found := FilterRepos(repos, args)
	if len(found) == 0 {
		b.reply(evt, "No repositories match.")
		return
	}
	const max = 50
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%d matching repositories:**\n\n", len(found))
	for i, r := range found {
		if i == max {
			fmt.Fprintf(&sb, "\n_…and %d more; narrow the search._\n", len(found)-max)
			break
		}
		sb.WriteString(repoLine(r))
		if r.Notes != "" {
			fmt.Fprintf(&sb, "  _%s_\n", oneLineMD(r.Notes, 120))
		}
	}
	b.reply(evt, sb.String())
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
	d := b.arch.Details(b.ctx, r, 5)
	nrel, _ := b.store.ReleaseCount(b.ctx, r.ID)
	latest, published, _ := b.store.LatestRelease(b.ctx, r.ID)

	var sb strings.Builder
	fmt.Fprintf(&sb, "**#%d** %s", r.ID, r.URL)
	for _, t := range r.Tags {
		sb.WriteString(" · `" + t + "`")
	}
	sb.WriteString("\n\n")
	if r.Notes != "" {
		fmt.Fprintf(&sb, "> %s\n\n", strings.ReplaceAll(r.Notes, "\n", "\n> "))
	}
	fmt.Fprintf(&sb, "- State: %s\n", repoState(r))
	if r.Upstream != "" {
		fmt.Fprintf(&sb, "- Upstream: %s since %s\n", r.UpstreamDetail, r.UpstreamSince.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(&sb, "- Added by %s, %s\n", r.AddedBy, ago(r.AddedAt))
	fmt.Fprintf(&sb, "- Last checked: %s · last changed: %s\n", ago(r.LastChecked), ago(r.LastChanged))
	if d.Branch != "" {
		fmt.Fprintf(&sb, "- Default branch: `%s`\n", d.Branch)
	}
	if nrel > 0 {
		fmt.Fprintf(&sb, "- Releases archived: %d (latest: %s, %s)\n", nrel, latest, published.Format("2006-01-02"))
	} else {
		sb.WriteString("- Releases archived: 0\n")
	}
	if n := len(d.Snapshots); n > 0 {
		refs := 0
		for _, s := range d.Snapshots {
			refs += s.Refs
		}
		fmt.Fprintf(&sb, "- History snapshots: %d (%d refs preserved from rewrites/deletions, latest `refs/archive/%s`)\n",
			n, refs, d.Snapshots[n-1].Stamp)
	}
	fmt.Fprintf(&sb, "- Disk usage: %s (`%s`)\n", gitops.HumanSize(gitops.DirSize(b.arch.RepoDir(r))), r.RelDir)
	if u := b.cfg.HTTP.BrowseURL(r.RelDir); u != "" {
		fmt.Fprintf(&sb, "- Browse: %s\n", u)
	}
	if r.LastError != "" {
		fmt.Fprintf(&sb, "- Last error: `%s`\n", strings.ReplaceAll(oneLineMD(r.LastError, 300), "`", "'"))
	}
	if len(d.Commits) > 0 {
		sb.WriteString("\n**Recent commits:**\n\n")
		for _, c := range d.Commits {
			fmt.Fprintf(&sb, "- `%s` %s — %s, %s\n", c.Hash, oneLineMD(c.Subject, 100), oneLineMD(c.Author, 40), ago(c.Time))
		}
	}
	b.reply(evt, sb.String())
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
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Disk usage:** %s in %d repositories%s\n\n", gitops.HumanSize(total), len(repos), freeSpaceText(b.cfg.StorageDir))
	const max = 25
	for i, r := range repos {
		if i == max {
			fmt.Fprintf(&sb, "\n_…and %d smaller ones._\n", len(repos)-max)
			break
		}
		pct := 0.0
		if total > 0 {
			pct = float64(r.SizeBytes) * 100 / float64(total)
		}
		fmt.Fprintf(&sb, "- %s (%.0f%%) — **#%d** %s\n", gitops.HumanSize(r.SizeBytes), pct, r.ID, r.URL)
	}
	sb.WriteString("\n_Sizes are measured after each change._")
	b.reply(evt, sb.String())
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
	b.reply(evt, cycleReport(results))
}

// oneLineMD flattens text to a single line, trimmed to max runes.
func oneLineMD(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
