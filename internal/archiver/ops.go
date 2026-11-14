package archiver

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

const diskLowKey = "disk_low"

// CheckDisk alerts the room (once) when free space in the storage dir drops below
// disk_warn_free, and posts a notice when it recovers.
func (a *Archiver) CheckDisk(ctx context.Context) {
	if a.cfg.DiskWarnFree <= 0 {
		return
	}
	free, total, err := FreeSpace(a.cfg.StorageDir)
	if err != nil {
		a.log.Debug().Err(err).Msg("Free space check failed")
		return
	}
	wasLow, _ := a.store.Get(ctx, diskLowKey)
	low := free < uint64(a.cfg.DiskWarnFree)
	switch {
	case low && wasLow == "":
		a.log.Warn().Uint64("free", free).Msg("Storage is running low")
		_ = a.store.Set(ctx, diskLowKey, "1")
		a.Alert(ctx, fmt.Sprintf("💾 Storage is running low: **%s free** of %s (warning below %s). New clones and releases may start failing.",
			gitops.HumanSize(int64(free)), gitops.HumanSize(int64(total)), gitops.HumanSize(a.cfg.DiskWarnFree)))
	case !low && wasLow != "":
		_ = a.store.Set(ctx, diskLowKey, "")
		a.Notify(ctx, fmt.Sprintf("💾 Storage is fine again: %s free of %s.", gitops.HumanSize(int64(free)), gitops.HumanSize(int64(total))))
	}
}

// WriteMetrics writes Prometheus text-format metrics.
func (a *Archiver) WriteMetrics(ctx context.Context, w io.Writer) error {
	repos, err := a.store.Repos(ctx)
	if err != nil {
		return err
	}
	releases, err := a.store.ReleaseCounts(ctx)
	if err != nil {
		return err
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })

	var b strings.Builder
	metric := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	var failing, gone int
	var used int64
	for _, r := range repos {
		if r.FailCount > 0 {
			failing++
		}
		if r.Upstream != "" {
			gone++
		}
		used += r.SizeBytes
	}
	metric("archivist_repos", "gauge", "Number of tracked repositories.")
	fmt.Fprintf(&b, "archivist_repos %d\n", len(repos))
	metric("archivist_repos_failing", "gauge", "Repositories whose last check failed.")
	fmt.Fprintf(&b, "archivist_repos_failing %d\n", failing)
	metric("archivist_repos_gone_upstream", "gauge", "Repositories that disappeared upstream.")
	fmt.Fprintf(&b, "archivist_repos_gone_upstream %d\n", gone)
	metric("archivist_storage_used_bytes", "gauge", "Disk space used by all repositories (as of their last change).")
	fmt.Fprintf(&b, "archivist_storage_used_bytes %d\n", used)
	if free, total, err := FreeSpace(a.cfg.StorageDir); err == nil {
		metric("archivist_storage_free_bytes", "gauge", "Free space on the storage filesystem.")
		fmt.Fprintf(&b, "archivist_storage_free_bytes %d\n", free)
		metric("archivist_storage_size_bytes", "gauge", "Total size of the storage filesystem.")
		fmt.Fprintf(&b, "archivist_storage_size_bytes %d\n", total)
	}
	metric("archivist_checks_total", "counter", "Repository checks since start, by result.")
	fmt.Fprintf(&b, "archivist_checks_total{result=\"ok\"} %d\n", a.checksOK.Load())
	fmt.Fprintf(&b, "archivist_checks_total{result=\"error\"} %d\n", a.checksFailed.Load())
	if end := a.lastCycleEnd.Load(); end > 0 {
		metric("archivist_last_cycle_timestamp_seconds", "gauge", "End time of the last full check cycle.")
		fmt.Fprintf(&b, "archivist_last_cycle_timestamp_seconds %d\n", end)
		metric("archivist_last_cycle_duration_seconds", "gauge", "Duration of the last full check cycle.")
		fmt.Fprintf(&b, "archivist_last_cycle_duration_seconds %.3f\n", float64(a.lastCycleDur.Load())/1000)
	}

	perRepo := []struct {
		name, typ, help string
		val             func(i int) string
	}{
		{"archivist_repo_up", "gauge", "1 if the repository's last check succeeded.", func(i int) string { return b2s(repos[i].FailCount == 0) }},
		{"archivist_repo_failures", "gauge", "Consecutive failed checks.", func(i int) string { return fmt.Sprint(repos[i].FailCount) }},
		{"archivist_repo_gone_upstream", "gauge", "1 if the repository disappeared upstream.", func(i int) string { return b2s(repos[i].Upstream != "") }},
		{"archivist_repo_size_bytes", "gauge", "Disk space used by the repository archive.", func(i int) string { return fmt.Sprint(repos[i].SizeBytes) }},
		{"archivist_repo_releases", "gauge", "Archived releases.", func(i int) string { return fmt.Sprint(releases[repos[i].ID]) }},
		{"archivist_repo_last_check_timestamp_seconds", "gauge", "Time of the last check.", func(i int) string {
			return fmt.Sprint(unixOr0(repos[i].LastChecked.Unix(), repos[i].LastChecked.IsZero()))
		}},
		{"archivist_repo_last_change_timestamp_seconds", "gauge", "Time the last change was archived.", func(i int) string {
			return fmt.Sprint(unixOr0(repos[i].LastChanged.Unix(), repos[i].LastChanged.IsZero()))
		}},
	}
	for _, m := range perRepo {
		metric(m.name, m.typ, m.help)
		for i, r := range repos {
			fmt.Fprintf(&b, "%s{id=\"%d\",repo=\"%s\"} %s\n", m.name, r.ID, labelEscape(r.Key), m.val(i))
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func b2s(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func unixOr0(v int64, zero bool) int64 {
	if zero {
		return 0
	}
	return v
}

var labelReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labelEscape(s string) string { return labelReplacer.Replace(s) }

// RepoDetails is git-level information about an archived repository.
type RepoDetails struct {
	Branch    string
	Commits   []gitops.Commit
	Snapshots []gitops.Snapshot
}

// Details reads the default branch, recent commits and history snapshots from the mirror.
func (a *Archiver) Details(ctx context.Context, repo *store.Repo, commits int) RepoDetails {
	mirror := a.mirrorDir(repo)
	if !dirExists(mirror) {
		return RepoDetails{}
	}
	d := RepoDetails{Branch: a.git.DefaultBranch(ctx, mirror)}
	d.Commits, _ = a.git.RecentCommits(ctx, mirror, commits)
	d.Snapshots, _ = a.git.Snapshots(ctx, mirror)
	return d
}
