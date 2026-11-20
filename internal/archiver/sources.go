package archiver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// maxSourceBody caps the size of a source's response.
const maxSourceBody = 32 << 20

// SourceEntry is one repository listed by a source.
type SourceEntry struct {
	URL    string
	Tags   []string
	Note   string
	Fields map[string]any // the entry's JSON object, for the announcement
}

// ParseSource reads a source's response: a JSON array of objects (the URL in urlField, tags
// from tagFields, a note from noteFields) or of URL strings, or plain text with one URL per
// line ("#" starts a comment).
func ParseSource(data []byte, src config.Source) ([]SourceEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("[")) {
		if bytes.HasPrefix(trimmed, []byte("{")) || bytes.HasPrefix(trimmed, []byte("<")) {
			return nil, errors.New("expected a JSON array or a list of URLs")
		}
		var out []SourceEntry
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				out = append(out, SourceEntry{URL: strings.Fields(line)[0]})
			}
		}
		return out, sc.Err()
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := make([]SourceEntry, 0, len(items))
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			out = append(out, SourceEntry{URL: s})
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(it, &obj); err != nil {
			continue
		}
		e := SourceEntry{Fields: obj}
		e.URL, _ = obj[src.URLField].(string)
		for _, f := range src.TagFields {
			e.Tags = append(e.Tags, stringsOf(obj[f])...)
		}
		var note []string
		for _, f := range src.NoteFields {
			note = append(note, stringsOf(obj[f])...)
		}
		e.Note = strings.Join(note, " · ")
		out = append(out, e)
	}
	return out, nil
}

// stringsOf returns the non-empty strings in a JSON value (a string or a list of them).
func stringsOf(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			out = append(out, s)
		}
	case []any:
		for _, x := range t {
			out = append(out, stringsOf(x)...)
		}
	}
	return out
}

// SyncResult is what one sync of a source changed.
type SyncResult struct {
	Listed int
	Added  []*store.Repo
	// AddedEntries are the source entries of Added, in the same order.
	AddedEntries []SourceEntry
	Delisted     []*store.Repo
	Relisted     []*store.Repo
	Invalid      []string // URLs that couldn't be parsed
	// SkippedDelist is set when too few entries came back to trust the list for delisting.
	SkippedDelist bool
}

func (r SyncResult) Empty() bool {
	return len(r.Added) == 0 && len(r.Delisted) == 0 && len(r.Relisted) == 0
}

// runSource syncs one source now and then every interval until ctx is cancelled.
func (a *Archiver) runSource(ctx context.Context, src config.Source) {
	log := a.log.With().Str("source", src.Name).Logger()
	// Give the bot a moment to connect, so the first sync's message reaches the room.
	select {
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return
	}
	for {
		res, err := a.SyncSource(ctx, src)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn().Err(err).Msg("Source sync failed")
		case err == nil:
			log.Debug().Int("listed", res.Listed).Int("added", len(res.Added)).Msg("Source synced")
			for _, msg := range SyncMessages(src, res) {
				a.Notify(ctx, msg)
			}
		}
		select {
		case <-time.After(src.Interval):
		case <-ctx.Done():
			return
		}
	}
}

func (a *Archiver) fetchSource(ctx context.Context, src config.Source) ([]byte, error) {
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	req.Header.Set("User-Agent", "matrix-archivist")
	// A plain client: the source URL comes from the config, so it may be on the local network
	// (e.g. another container).
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %s", src.URL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSourceBody {
		return nil, fmt.Errorf("%s: response larger than %s", src.URL, gitops.HumanSize(maxSourceBody))
	}
	return data, nil
}

// SyncSource fetches a source and adds the repositories it lists that aren't tracked yet
// (except ones removed by hand). Repositories it added earlier that it no longer lists are
// tagged "delisted" but kept; the tag goes away if they come back. They are cloned by the
// check cycle.
func (a *Archiver) SyncSource(ctx context.Context, src config.Source) (SyncResult, error) {
	data, err := a.fetchSource(ctx, src)
	if err != nil {
		return SyncResult{}, err
	}
	entries, err := ParseSource(data, src)
	if err != nil {
		return SyncResult{}, fmt.Errorf("%s: %w", src.URL, err)
	}
	return a.applySource(ctx, src, entries)
}

func (a *Archiver) applySource(ctx context.Context, src config.Source, entries []SourceEntry) (SyncResult, error) {
	var res SyncResult
	ignored, err := a.store.Ignored(ctx)
	if err != nil {
		return res, err
	}
	repos, err := a.store.Repos(ctx)
	if err != nil {
		return res, err
	}
	byKey := map[string]*store.Repo{}
	owned := 0
	for _, r := range repos {
		byKey[r.Key] = r
		if r.Source == src.Name {
			owned++
		}
	}
	listed := map[string]bool{}
	for _, e := range entries {
		u, err := gitops.ParseRepoURL(e.URL)
		if err != nil {
			res.Invalid = append(res.Invalid, e.URL)
			continue
		}
		key := u.Key()
		if listed[key] {
			continue
		}
		listed[key] = true
		if r := byKey[key]; r != nil {
			if r.Source == src.Name && slices.Contains(r.Tags, store.TagDelisted) {
				tags := slices.DeleteFunc(slices.Clone(r.Tags), func(t string) bool { return t == store.TagDelisted })
				if err := a.store.SetTags(ctx, r.ID, tags); err != nil {
					return res, err
				}
				res.Relisted = append(res.Relisted, r)
			}
			continue
		}
		if ignored[key] {
			continue
		}
		r, err := a.store.AddRepoMode(ctx, u.Raw, key, u.RelDir(), "source:"+src.Name, src.Mode, src.Name)
		if errors.Is(err, store.ErrDuplicate) {
			continue // same storage key added meanwhile (e.g. by a command)
		} else if err != nil {
			return res, err
		}
		if tags := append(slices.Clone(src.Tags), e.Tags...); len(tags) > 0 {
			_ = a.store.SetTags(ctx, r.ID, tags)
		}
		if e.Note != "" {
			_ = a.store.SetNotes(ctx, r.ID, oneLine(e.Note))
		}
		byKey[key] = r
		res.Added = append(res.Added, r)
		res.AddedEntries = append(res.AddedEntries, e)
	}
	res.Listed = len(listed)

	// A source that suddenly lists far fewer repositories is more likely broken than pruned.
	if len(listed) == 0 || len(listed) < owned/2 {
		res.SkippedDelist = owned > 0
		if res.SkippedDelist {
			a.log.Warn().Str("source", src.Name).Int("listed", len(listed)).Int("known", owned).
				Msg("Source lists less than half of the repositories it added; not marking any as delisted")
		}
		return res, nil
	}
	for _, r := range repos {
		if r.Source != src.Name || listed[r.Key] || slices.Contains(r.Tags, store.TagDelisted) {
			continue
		}
		if err := a.store.SetTags(ctx, r.ID, append(slices.Clone(r.Tags), store.TagDelisted)); err != nil {
			return res, err
		}
		res.Delisted = append(res.Delisted, r)
	}
	return res, nil
}

// announceMax is how many repositories one sync announces one by one. Beyond that (e.g. the
// first sync of a big list) they are announced in a single message.
const announceMax = 10

// SyncMessages are the room notices about a source sync: one per added repository (or a single
// one when many were added at once), and one about delisted and relisted repositories.
func SyncMessages(src config.Source, res SyncResult) []string {
	var out []string
	if n := len(res.Added); n > 0 && n <= announceMax {
		for i, r := range res.Added {
			out = append(out, Announce(src, r, res.AddedEntries[i]))
		}
		res.Added = nil
	}
	if text := SyncText(src, res); text != "" {
		out = append(out, text)
	}
	return out
}

// Announce renders the notice about one repository added by a source.
func Announce(src config.Source, r *store.Repo, e SourceEntry) string {
	if src.AnnounceTemplate != nil {
		data := map[string]any{}
		for k, v := range e.Fields {
			data[k] = v
		}
		data["repo"], data["id"], data["mode"] = r.URL, r.ID, r.Mode
		var b strings.Builder
		if err := src.AnnounceTemplate.Execute(&b, data); err == nil {
			text := strings.Join(strings.Fields(strings.ReplaceAll(b.String(), "<no value>", "")), " ")
			return fmt.Sprintf("📥 %s (**#%d**, %s archive)", text, r.ID, r.Mode)
		}
	}
	return fmt.Sprintf("📥 Archiving **#%d** %s from source **%s** (%s mode).", r.ID, r.URL, src.Name, r.Mode)
}

// SyncText is the grouped room message about a source sync ("" if nothing to say).
func SyncText(src config.Source, res SyncResult) string {
	var b strings.Builder
	list := func(repos []*store.Repo) {
		const max = 10
		for i, r := range repos {
			if i == max {
				fmt.Fprintf(&b, "- …and %d more\n", len(repos)-max)
				break
			}
			fmt.Fprintf(&b, "- **#%d** %s\n", r.ID, r.URL)
		}
	}
	if n := len(res.Added); n > 0 {
		fmt.Fprintf(&b, "📥 Source **%s**: %d new %s queued for archiving (%s mode).\n\n", src.Name, n, plural(n, "repository", "repositories"), src.Mode)
		list(res.Added)
	}
	if n := len(res.Delisted); n > 0 {
		fmt.Fprintf(&b, "\n🏷️ Source **%s** no longer lists %d %s. The archive is kept and tagged `%s`.\n\n", src.Name, n, plural(n, "repository", "repositories"), store.TagDelisted)
		list(res.Delisted)
	}
	if n := len(res.Relisted); n > 0 {
		fmt.Fprintf(&b, "\n🏷️ Source **%s** lists %d %s again.\n\n", src.Name, n, plural(n, "repository", "repositories"))
		list(res.Relisted)
	}
	return strings.TrimSpace(b.String())
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
