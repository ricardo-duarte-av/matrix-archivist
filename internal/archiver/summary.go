package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

const lastSummaryKey = "last_summary"

// NextSummary returns the next occurrence of hh:mm in loc strictly after now.
func NextSummary(now time.Time, hour, minute int, loc *time.Location) time.Time {
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
	if !next.After(n) {
		next = time.Date(n.Year(), n.Month(), n.Day()+1, hour, minute, 0, 0, loc)
	}
	return next
}

func (a *Archiver) runSummaries(ctx context.Context) {
	if !a.cfg.Summary.Enabled {
		return
	}
	s := a.cfg.Summary
	for {
		next := NextSummary(time.Now(), s.Hour, s.Minute, s.Location)
		a.log.Debug().Time("next", next).Msg("Next summary scheduled")
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		a.PostSummary(ctx, true)
		_ = a.store.PruneEvents(ctx, time.Now().AddDate(0, 0, -60))
	}
}

// PostSummary builds and posts the summary of events since the last scheduled summary.
// When scheduled is true, the "last summary" marker is advanced.
func (a *Archiver) PostSummary(ctx context.Context, scheduled bool) {
	since := time.Now().Add(-24 * time.Hour)
	if v, _ := a.store.Get(ctx, lastSummaryKey); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = time.Unix(ts, 0)
		}
	}
	now := time.Now()
	text, empty, err := a.BuildSummary(ctx, since)
	if err != nil {
		a.log.Err(err).Msg("Failed to build summary")
		return
	}
	if scheduled {
		_ = a.store.Set(ctx, lastSummaryKey, strconv.FormatInt(now.Unix(), 10))
		if empty && a.cfg.Summary.SkipIfEmpty {
			return
		}
	}
	a.Notify(ctx, text)
}

type repoActivity struct {
	updates  []gitops.Change
	releases []string
	added    string
}

// BuildSummary renders a Markdown summary of events since the given time.
func (a *Archiver) BuildSummary(ctx context.Context, since time.Time) (text string, empty bool, err error) {
	events, err := a.store.EventsSince(ctx, since)
	if err != nil {
		return "", false, err
	}
	repos, err := a.store.Repos(ctx)
	if err != nil {
		return "", false, err
	}
	byID := map[int64]*store.Repo{}
	for _, r := range repos {
		byID[r.ID] = r
	}

	act := map[int64]*repoActivity{}
	get := func(id int64) *repoActivity {
		if act[id] == nil {
			act[id] = &repoActivity{}
		}
		return act[id]
	}
	for _, e := range events {
		ra := get(e.RepoID)
		switch e.Kind {
		case store.EventUpdate:
			var c gitops.Change
			if json.Unmarshal([]byte(e.Detail), &c) == nil {
				ra.updates = append(ra.updates, c)
			}
		case store.EventRelease:
			ra.releases = append(ra.releases, e.Detail)
		case store.EventAdded:
			ra.added = e.Detail
		}
	}

	loc := a.cfg.Summary.Location
	var b strings.Builder
	fmt.Fprintf(&b, "### 📦 Archivist summary\n\nSince %s\n\n", since.In(loc).Format("2006-01-02 15:04 MST"))

	ids := make([]int64, 0, len(act))
	for id := range act {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var updated, failing []string
	for _, id := range ids {
		ra := act[id]
		r := byID[id]
		if r == nil {
			continue
		}
		var parts []string
		if ra.added != "" {
			parts = append(parts, "🆕 "+ra.added)
		}
		if len(ra.updates) > 0 {
			parts = append(parts, MergeChanges(ra.updates).Summary())
		}
		if len(ra.releases) > 0 {
			parts = append(parts, "🏷️ "+releasesText(ra.releases))
		}
		if len(parts) > 0 {
			updated = append(updated, fmt.Sprintf("- **#%d** %s — %s", id, r.URL, strings.Join(parts, "; ")))
		}
	}
	// Every repository that is failing right now, whether or not it had events in the window.
	for _, r := range repos {
		if r.FailCount > 0 {
			failing = append(failing, fmt.Sprintf("- **#%d** %s — %d failures in a row, last: `%s`", r.ID, r.URL, r.FailCount, oneLine(r.LastError)))
		}
	}

	if len(updated) == 0 {
		b.WriteString("No repositories changed.\n")
	} else {
		fmt.Fprintf(&b, "**Updated (%d):**\n\n%s\n", len(updated), strings.Join(updated, "\n"))
	}
	if len(failing) > 0 {
		fmt.Fprintf(&b, "\n**Failing (%d):**\n\n%s\n", len(failing), strings.Join(failing, "\n"))
	}
	fmt.Fprintf(&b, "\n_%d repositories tracked._", len(repos))
	return b.String(), len(updated) == 0 && len(failing) == 0, nil
}

// MergeChanges combines several consecutive changes into one.
func MergeChanges(changes []gitops.Change) gitops.Change {
	var m gitops.Change
	union := func(dst *[]string, src []string) {
		seen := map[string]bool{}
		for _, v := range *dst {
			seen[v] = true
		}
		for _, v := range src {
			if !seen[v] {
				seen[v] = true
				*dst = append(*dst, v)
			}
		}
	}
	for _, c := range changes {
		m.NewCommits += c.NewCommits
		union(&m.NewBranches, c.NewBranches)
		union(&m.UpdatedBranches, c.UpdatedBranches)
		union(&m.DeletedBranches, c.DeletedBranches)
		union(&m.NewTags, c.NewTags)
		union(&m.DeletedTags, c.DeletedTags)
	}
	return m
}
