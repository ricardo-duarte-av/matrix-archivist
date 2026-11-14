package bot

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/archiver"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

type command struct {
	usage      string
	desc       string
	privileged bool
	run        func(b *Bot, evt *event.Event, args []string)
}

var commands map[string]command

var helpOrder = []string{"add", "remove", "list", "search", "info", "note", "tag", "untag", "refresh", "status", "usage", "summary", "export", "import", "help"}

func init() {
	commands = map[string]command{
		"add":     {"add <url> [url…]", "Add git repositories to the archive", true, (*Bot).cmdAdd},
		"remove":  {"remove <#id|url> [--purge]", "Stop tracking a repository (`--purge` also deletes its files)", true, (*Bot).cmdRemove},
		"list":    {"list [tag]", "List tracked repositories, optionally only those with a tag", false, (*Bot).cmdList},
		"search":  {"search <text|#tag>…", "Search repositories by URL, note and tags", false, (*Bot).cmdSearch},
		"info":    {"info <#id|url>", "Details about a repository: state, recent commits, releases, snapshots", false, (*Bot).cmdInfo},
		"note":    {"note <#id> [text|--clear]", "Show, set or clear a note on a repository (why it is archived)", true, (*Bot).cmdNote},
		"tag":     {"tag <#id> <tag>…", "Add tags to a repository", true, (*Bot).cmdTag},
		"untag":   {"untag <#id> <tag>…", "Remove tags from a repository", true, (*Bot).cmdUntag},
		"refresh": {"refresh <#id|url|all>", "Check a repository for updates now", true, (*Bot).cmdRefresh},
		"status":  {"status [#id|url]", "Overall status (or details about one repository)", false, (*Bot).cmdStatus},
		"usage":   {"usage", "Disk usage per repository and free space", false, (*Bot).cmdUsage},
		"summary": {"summary", "Post the summary of changes since the last scheduled summary", false, (*Bot).cmdSummary},
		"export":  {"export", "Upload the repository list (with notes and tags) as a JSON file", false, (*Bot).cmdExport},
		"import":  {"import", "Reply to an exported file (or a text file of URLs) to add its repositories", true, (*Bot).cmdImport},
		"help":    {"help", "Show this help", false, (*Bot).cmdHelp},
	}
	aliases := map[string]string{"rm": "remove", "del": "remove", "ls": "list", "update": "refresh", "pull": "refresh", "find": "search", "du": "usage"}
	for a, c := range aliases {
		commands[a] = commands[c]
	}
}

func (b *Bot) handle(evt *event.Event, args []string) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error().Any("panic", r).Msg("Command panicked")
			b.reply(evt, "💥 Internal error while running that command.")
		}
	}()
	cmd, ok := commands[args[0]]
	if !ok {
		b.reply(evt, fmt.Sprintf("Unknown command `%s`. Try `%s help`.", args[0], b.cfg.CommandPrefix))
		return
	}
	if cmd.privileged {
		if lvl := b.powerLevel(b.ctx, evt.Sender); lvl < b.cfg.MinPowerLevel {
			b.reply(evt, fmt.Sprintf("⛔ You need power level %d for `%s` (you have %d).", b.cfg.MinPowerLevel, args[0], lvl))
			return
		}
	}
	cmd.run(b, evt, args[1:])
}

func (b *Bot) cmdHelp(evt *event.Event, _ []string) {
	var sb strings.Builder
	sb.WriteString("**Archivist commands**\n\n")
	for _, name := range helpOrder {
		c := commands[name]
		lock := ""
		if c.privileged {
			lock = fmt.Sprintf(" _(PL ≥ %d)_", b.cfg.MinPowerLevel)
		}
		fmt.Fprintf(&sb, "- `%s %s` — %s%s\n", b.cfg.CommandPrefix, c.usage, c.desc, lock)
	}
	fmt.Fprintf(&sb, "\nRepositories are checked every %s.", b.cfg.CheckInterval)
	if b.cfg.Summary.Enabled {
		fmt.Fprintf(&sb, " A summary is posted daily at %s (%s).", b.cfg.Summary.Time, b.cfg.Summary.Timezone)
	}
	b.reply(evt, sb.String())
}

func (b *Bot) cmdAdd(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s add <url>`", b.cfg.CommandPrefix))
		return
	}
	var lines []string
	var added []*store.Repo
	for _, raw := range args {
		u, err := gitops.ParseRepoURL(raw)
		if err != nil {
			lines = append(lines, fmt.Sprintf("- ❌ `%s`: %v", raw, err))
			continue
		}
		repo, err := b.store.AddRepo(b.ctx, u.Raw, u.Key(), u.RelDir(), evt.Sender.String())
		switch {
		case errors.Is(err, store.ErrDuplicate):
			lines = append(lines, fmt.Sprintf("- ℹ️ already tracked as **#%d** %s", repo.ID, repo.URL))
		case err != nil:
			lines = append(lines, fmt.Sprintf("- ❌ `%s`: %v", raw, err))
		default:
			lines = append(lines, fmt.Sprintf("- ✅ added **#%d** %s — cloning…", repo.ID, repo.URL))
			added = append(added, repo)
		}
	}
	b.reply(evt, strings.Join(lines, "\n"))
	if len(added) == 0 {
		return
	}
	done := b.progress(evt)
	var wg sync.WaitGroup
	var failed atomic.Bool
	for _, repo := range added {
		wg.Add(1)
		go func(repo *store.Repo) {
			defer wg.Done()
			res := b.arch.Check(b.ctx, repo.ID)
			if res.Err != nil {
				failed.Store(true)
			}
			b.reply(evt, fmt.Sprintf("**#%d** %s: %s", repo.ID, repo.URL, res.Text()))
		}(repo)
	}
	wg.Wait()
	done(!failed.Load())
}

// resolve finds a repo by "#3", "3" or URL.
func (b *Bot) resolve(arg string) (*store.Repo, error) {
	if n, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64); err == nil {
		r, err := b.store.Repo(b.ctx, n)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("no repository with index #%d", n)
		}
		return r, err
	}
	u, err := gitops.ParseRepoURL(arg)
	if err != nil {
		return nil, err
	}
	r, err := b.store.RepoByKey(b.ctx, u.Key())
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s is not tracked", arg)
	}
	return r, err
}

func (b *Bot) cmdRemove(evt *event.Event, args []string) {
	purge := false
	var targets []string
	for _, a := range args {
		if a == "--purge" || a == "-p" {
			purge = true
		} else {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s remove <#id|url> [--purge]`", b.cfg.CommandPrefix))
		return
	}
	var lines []string
	for _, t := range targets {
		repo, err := b.resolve(t)
		if err != nil {
			lines = append(lines, fmt.Sprintf("- ❌ %v", err))
			continue
		}
		if err := b.arch.Remove(b.ctx, repo, purge); err != nil {
			lines = append(lines, fmt.Sprintf("- ❌ #%d: %v", repo.ID, err))
			continue
		}
		if purge {
			lines = append(lines, fmt.Sprintf("- 🗑️ removed **#%d** %s and deleted its files", repo.ID, repo.URL))
		} else {
			lines = append(lines, fmt.Sprintf("- 🗑️ removed **#%d** %s (files kept in `%s`)", repo.ID, repo.URL, repo.RelDir))
		}
	}
	b.reply(evt, strings.Join(lines, "\n"))
}

func (b *Bot) cmdList(evt *event.Event, args []string) {
	repos, err := b.store.Repos(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	if len(repos) == 0 {
		b.reply(evt, fmt.Sprintf("No repositories tracked yet. Add one with `%s add <url>`.", b.cfg.CommandPrefix))
		return
	}
	title := "Tracked repositories"
	if len(args) > 0 {
		tag := store.NormalizeTag(args[0])
		repos = FilterRepos(repos, []string{"#" + tag})
		title = fmt.Sprintf("Repositories tagged `%s`", tag)
		if len(repos) == 0 {
			b.reply(evt, fmt.Sprintf("No repositories are tagged `%s`.", tag))
			return
		}
	}
	const perMsg = 50
	for i := 0; i < len(repos); i += perMsg {
		end := min(i+perMsg, len(repos))
		var sb strings.Builder
		if i == 0 {
			fmt.Fprintf(&sb, "**%s (%d):**\n\n", title, len(repos))
		}
		for _, r := range repos[i:end] {
			sb.WriteString(repoLine(r))
		}
		b.reply(evt, sb.String())
	}
}

// repoLine is a list entry: "- **#3** url — state · `tag`\n".
func repoLine(r *store.Repo) string {
	line := fmt.Sprintf("- **#%d** %s — %s", r.ID, r.URL, repoState(r))
	for _, t := range r.Tags {
		line += " · `" + t + "`"
	}
	return line + "\n"
}

func repoState(r *store.Repo) string {
	switch {
	case r.Upstream == store.UpstreamLegal:
		return fmt.Sprintf("⚖️ taken down upstream %s, archive preserved", ago(r.UpstreamSince))
	case r.Upstream == store.UpstreamBlocked:
		return fmt.Sprintf("🚫 blocked upstream %s, archive preserved", ago(r.UpstreamSince))
	case r.Upstream != "":
		return fmt.Sprintf("🪦 gone upstream %s, archive preserved", ago(r.UpstreamSince))
	case r.FailCount > 0 && !r.Cloned:
		return fmt.Sprintf("❌ clone failing (%d×)", r.FailCount)
	case r.FailCount > 0:
		return fmt.Sprintf("⚠️ failing (%d×), last ok update %s", r.FailCount, ago(r.LastChanged))
	case !r.Cloned:
		return "⏳ not cloned yet"
	default:
		return fmt.Sprintf("✅ checked %s, changed %s", ago(r.LastChecked), ago(r.LastChanged))
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func (b *Bot) cmdRefresh(evt *event.Event, args []string) {
	if len(args) == 0 {
		b.reply(evt, fmt.Sprintf("Usage: `%s refresh <#id|url|all>`", b.cfg.CommandPrefix))
		return
	}
	if strings.EqualFold(args[0], "all") {
		done := b.progress(evt)
		results, ran := b.arch.RunCycle(b.ctx)
		if !ran {
			done(true)
			b.reply(evt, "A check cycle is already running; results will appear in the next summary.")
			return
		}
		ok := true
		for _, r := range results {
			ok = ok && r.Err == nil
		}
		done(ok)
		b.reply(evt, cycleReport(results))
		return
	}
	var repos []*store.Repo
	var lines []string
	for _, a := range args {
		r, err := b.resolve(a)
		if err != nil {
			lines = append(lines, "- ❌ "+err.Error())
			continue
		}
		repos = append(repos, r)
	}
	ok := len(lines) == 0
	if len(repos) > 0 {
		done := b.progress(evt)
		for _, r := range repos {
			res := b.arch.Check(b.ctx, r.ID)
			ok = ok && res.Err == nil
			lines = append(lines, fmt.Sprintf("- **#%d** %s: %s", r.ID, r.URL, res.Text()))
		}
		done(ok)
	}
	b.reply(evt, strings.Join(lines, "\n"))
}

func cycleReport(results []archiver.Result) string {
	var changed, failed []string
	for _, r := range results {
		if r.Repo == nil {
			continue
		}
		line := fmt.Sprintf("- **#%d** %s: %s", r.Repo.ID, r.Repo.URL, r.Text())
		if r.Err != nil {
			failed = append(failed, line)
		} else if r.Changed() {
			changed = append(changed, line)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Refreshed %d repositories: %d changed, %d failed.\n", len(results), len(changed), len(failed))
	if len(changed) > 0 {
		sb.WriteString("\n" + strings.Join(changed, "\n") + "\n")
	}
	if len(failed) > 0 {
		sb.WriteString("\n" + strings.Join(failed, "\n") + "\n")
	}
	return sb.String()
}

func (b *Bot) cmdStatus(evt *event.Event, args []string) {
	if len(args) > 0 {
		b.cmdInfo(evt, args)
		return
	}
	repos, err := b.store.Repos(b.ctx)
	if err != nil {
		b.reply(evt, "❌ "+err.Error())
		return
	}
	failing, gone := 0, 0
	var used int64
	for _, r := range repos {
		if r.Upstream != "" {
			gone++
		} else if r.FailCount > 0 {
			failing++
		}
		used += r.SizeBytes
	}
	var sb strings.Builder
	sb.WriteString("**Archivist status**\n\n")
	fmt.Fprintf(&sb, "- Repositories: %d (%d failing, %d gone upstream)\n", len(repos), failing, gone)
	fmt.Fprintf(&sb, "- Storage used: %s%s\n", gitops.HumanSize(used), freeSpaceText(b.cfg.StorageDir))
	fmt.Fprintf(&sb, "- Check interval: %s\n", b.cfg.CheckInterval)
	if u := b.cfg.HTTP.BrowseURL(""); u != "" {
		fmt.Fprintf(&sb, "- Browse: %s\n", u)
	}
	if b.cfg.Summary.Enabled {
		s := b.cfg.Summary
		next := archiver.NextSummary(time.Now(), s.Hour, s.Minute, s.Location)
		fmt.Fprintf(&sb, "- Next summary: %s\n", next.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(&sb, "- Uptime: %s\n", time.Since(b.start).Round(time.Second))
	b.reply(evt, sb.String())
}

func (b *Bot) cmdSummary(_ *event.Event, _ []string) {
	b.arch.PostSummary(b.ctx, false)
}
