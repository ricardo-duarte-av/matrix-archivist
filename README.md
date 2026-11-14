# matrix-archivist

A Matrix bot that keeps a local archive of git repositories and their releases.
It lives in one room and takes commands from it, checks every repository for updates on a schedule,
and posts a daily summary of what changed.

- **Full git archive:** for every repository it keeps a bare clone with all branches and tags (`mirror.git`) plus a browsable checkout of the default branch (`checkout/`).
- **Releases:** archives release notes, raw metadata and downloadable assets from **GitHub**, **Gitea / Forgejo / Codeberg** and **GitLab**, including self-hosted instances, which are detected automatically.
- **End-to-end encryption:** works in encrypted rooms, with pure-Go crypto (no libolm or CGO).
- **Permissions:** commands that change things require a minimum room power level.
- **Takedown-aware:** when a repository disappears upstream (deleted, made private, DMCA'd, blocked), the room gets an @room alert saying so, and the archive is kept.
- **History is never lost:** force-pushes, moved tags and deleted refs upstream are snapshotted under `refs/archive/` before they're overwritten.
- **Web file browser:** a read-only listing of the archive, also usable as a room widget. Download single files (resumable), or any folder as a zip.
- **Operations:** disk usage per repository, a low-disk @room alert, Prometheus metrics, and export/import of the repository list.
- **Single static binary:** multi-arch Docker image (amd64/arm64).

## Commands

The base command is set by `command_prefix` (default `!archivist`).

| Command | Who | |
|---|---|---|
| `!archivist add <url> [url…]` | PL ≥ `min_power_level` | Add repositories. Each is cloned straight away and the bot replies when done. |
| `!archivist remove <#id\|url> [--purge]` | PL ≥ `min_power_level` | Stop tracking. Files stay on disk unless `--purge` is given. |
| `!archivist list [tag]` | everyone | Tracked repositories with their `#id`, state and tags; optionally only those with a tag. |
| `!archivist search <text\|#tag>…` | everyone | Find repositories by URL, description, README, note, topic or tag. All terms must match; the best 50 are shown, name and description matches first. |
| `!archivist info <#id\|url>` | everyone | Details: state, upstream status, note, recent commits, latest release, history snapshots, disk usage. |
| `!archivist note <#id> [text\|--clear]` | PL ≥ `min_power_level` | Show, set or clear a note ("why we archived this"). |
| `!archivist tag <#id> <tag>…` / `untag …` | PL ≥ `min_power_level` | Add or remove tags. |
| `!archivist refresh <#id\|url\|all>` | PL ≥ `min_power_level` | Check now and reply with what changed. |
| `!archivist status` | everyone | Overall status: counts, storage, next summary. `status #id` is the same as `info`. |
| `!archivist usage` | everyone | Disk usage per repository, biggest first, and free space. |
| `!archivist summary` | everyone | Post the summary of changes since the last scheduled summary. |
| `!archivist export` | everyone | Upload the repository list, with notes and tags, as a JSON file. |
| `!archivist import` | PL ≥ `min_power_level` | Sent as a **reply** to an exported file, or to a text file with one URL per line: adds every repository and clones them. |
| `!archivist help` | everyone | Usage. |

Aliases: `ls`, `rm`/`del`, `update`/`pull`, `find`, `du`.

While `add`, `refresh` and `import` run, the command gets a ⏳ reaction, replaced by ✅ or ❌ when done.
Lists (`list`, `search`, `usage`, `refresh`) and `info`/`status` are sent as HTML tables, with a plain-text version for notifications and text-only clients. Long lists are split over several messages. Replies go to the main timeline; only a command sent inside a thread is answered in that thread.

### What each repository is about

Repository URLs say little ("TheItalianJob" may be a Mario port), so the bot collects a short description for each one:

- From the forge (GitHub, Gitea/Forgejo/Codeberg, GitLab): description, homepage and topics. Fetched at most once a day per repository.
- From the README in the checkout: the title, the first real paragraph, and community links (Discord, Matrix, Telegram, Reddit, Zulip, Libera, Patreon, Ko-fi). Re-read whenever the repository changes.

`list` and `search` show the description and links next to each repository, and `info` shows all of it. The README text, up to 64 KB, is stored so `search` can find words that appear only in a README. Repositories gone upstream keep what was collected. Existing installs fill this in on the next check cycle.

`#id` numbers are stable: removing #3 does not renumber #4.

Accepted URLs are `https://`, `http://`, `ssh://`, `git://` and `git@host:owner/repo`.
Local paths and `file://` are rejected.

## Storage layout

```
/data/
├── archivist.db            # repo list, archived releases, change history
├── crypto.db               # E2EE keys and Matrix state; back this up, don't delete it
└── repos/<host>/<owner>/<repo>/
    ├── mirror.git/         # bare clone: all branches + tags (+ refs/archive/ snapshots)
    ├── checkout/           # working tree of the default branch
    └── releases/<tag>/
        ├── notes.md
        ├── release.json    # raw API response
        └── assets/…
```

To restore a repository from the archive, run `git clone /data/repos/github.com/owner/repo/mirror.git`.

## Takedowns and rewritten history

**Disappearing repositories.** When a check of an archived repository fails, the bot asks the forge's API (or, for other hosts, the repository's web page) whether it still exists:

| Upstream answer | Alert |
|---|---|
| 404 / 410 | 🪦 disappeared upstream: deleted, renamed or made private |
| 451, or a DMCA block | ⚖️ taken down upstream, with the notice link when GitHub provides one |
| 403 "Repository access blocked" | 🚫 blocked upstream |

The alert is an **@room** message sent right away, not after `failure_alert_threshold` failures. It shows the archive's size, release count and a Browse link. The bot keeps checking. If the repository comes back, it posts ♻️ and resumes updating. Repositories that are gone stay listed in the daily summary under "Gone upstream, preserved here". Network errors, rate limits and server errors are treated as ordinary failures, not takedowns.

**Rewritten history.** Before each fetch is applied, the bot compares the refs. When upstream force-pushes a branch, moves a tag, or deletes a branch or tag (deleting only matters with `prune_deleted: true`), the old tip is saved as:

```
refs/archive/<UTC time>/heads/<branch>
refs/archive/<UTC time>/tags/<tag>
```

Nothing that was ever archived becomes unreachable. A rewrite of the default branch, or a moved or deleted tag, triggers an immediate notice. Rewrites of other branches only appear in the summary. `!archivist info` lists the snapshots. To look at one:

```sh
git -C mirror.git for-each-ref refs/archive/                 # list snapshots
git -C mirror.git log refs/archive/20261114T093000Z/heads/main # history before the rewrite
# Check out the pre-rewrite state in a fresh clone:
git clone mirror.git restored
git -C restored fetch origin refs/archive/20261114T093000Z/heads/main
git -C restored checkout -b before-rewrite FETCH_HEAD
```

## Web file browser

When `http.enabled` is true, the bot serves a read-only browser for `storage_dir` on `http.port` (default 8080):

| URL | What it does |
|---|---|
| `/browse/<path>/` | Folder listing |
| `/download/<path>` | Download one file. Supports resume (HTTP Range). |
| `/zip/<path>` | Download a folder as a zip. It is built on the fly, so no temp files are written. |
| `/healthz` | Liveness check, used by the Docker `HEALTHCHECK`. |

- **Static files:** the CSS, icons and template are compiled into the binary, so the container needs nothing extra.
- **Safe paths:** requests can't escape `storage_dir`. That includes `..` paths and symlinks pointing outside it.
- **Access:** there is no login by default. Set `http.username`/`http.password` for basic auth, or put the browser behind a reverse proxy that adds TLS and auth.
- **Links in the room:** with `http.public_url` set, `!archivist status` includes "Browse" links.
- **Changing the port:** keep the container side of the compose port mapping in step with `http.port`. The healthcheck reads the port from the config.
- **Metrics:** `/metrics` serves Prometheus metrics (`http.metrics`): repo counts, failing/gone repos, per-repo size, failures, release count and last check/change times, free disk space and check counters. It is behind the same basic auth as the rest.

### Room widget

With `http.widget.enabled` (default) and `http.public_url` set, the bot adds the browser to the room as a widget. In Element and SchildiChat it shows up under Room info → Extensions. Set `http.widget.pin: true` to show it at the top of the room for everyone. Other details:
- **Permissions:** the bot needs permission to change widgets in the room (Moderator by default). It logs a warning if it can't.
- **Unsupported clients:** clients without widget support, gomuks for example, simply ignore it.
- **Basic auth:** browsers often won't show a login prompt inside an embedded frame. With basic auth on, open the browser in a tab once (the session is then remembered), or use a reverse proxy with cookie-based auth.
- **Removing it:** set `http.widget.enabled: false`. The bot removes the widget at its next start.

## Running with Docker Compose

1. Create a Matrix account for the bot and a room for it, and note the room ID: in Element, Room settings → Advanced.
2. Copy and edit the config:
   ```sh
   cp config.example.yaml config.yaml
   mkdir data && sudo chown 1000:1000 data
   ```
3. Start it:
   ```sh
   docker compose up -d
   docker compose logs -f
   ```
4. Invite the bot to the room. It joins on its own.

The image is published to `ghcr.io/ricardo-duarte-av/matrix-archivist` by the GitHub Action in
`.github/workflows/docker.yml`:

| Trigger | Image tags |
|---|---|
| Push to `main` | `latest` |
| Tag `v1.2.3` | `1.2.3`, `1.2` |
| Every push | `sha-…` |

Pull requests are built but not pushed.

> GHCR packages are private by default. After the first push, either make the package public
> (GitHub → Packages → matrix-archivist → Package settings), or run `docker login ghcr.io` on the server.

### Private repositories

| Repository type | What to do |
|---|---|
| **HTTPS** | Put a token in the URL (`https://user:token@host/owner/repo`). The URL is shown in the room, so prefer the next option. |
| **SSH** | Mount a deploy key into `/home/archivist/.ssh` (see `docker-compose.yaml`) and add `git@host:owner/repo` URLs. |
| **Releases** | Set a `token` for the host under `forges:`. |

## Configuration

See [`config.example.yaml`](config.example.yaml); every option is commented there. The main ones:

| Option | Meaning |
|---|---|
| `matrix.*` | Homeserver, credentials and room. With `password`, the bot logs in and keeps its E2EE device in `crypto.db`. With `access_token`, you must also set the matching `device_id`. |
| `command_prefix` | Base command, e.g. `!archivist`, `!bot` or `!cloner`. |
| `min_power_level` | Power level needed for add, remove and refresh. |
| `check_interval` | How often every repository is checked. |
| `summary.time` / `summary.timezone` | When the daily summary is posted. |
| `releases.*` | Release archiving, asset downloads, per-asset size cap, prereleases, retention (`keep_last`, `max_repo_size`). |
| `forges` | API tokens and self-hosted forges. A GitHub token is strongly recommended: without one the API allows 60 requests/hour. |
| `prune_deleted` | Whether branches and tags deleted upstream are deleted from the archive too. Default `false`: they are kept. Either way they're snapshotted under `refs/archive/` first. |
| `disk_warn_free` | @room alert when free space in `storage_dir` drops below this (default `5GB`). A notice follows when it recovers. |
| `http.metrics`, `http.widget.*` | Prometheus endpoint, and the room widget. |

## Failure alerts

When a repository fails `failure_alert_threshold` checks in a row (default 5), the bot posts an **@room** alert with the last error. When it works again, it posts a normal message saying so.

For @room to notify anyone, the bot needs the room's "Notify everyone" power level, which is 50 by default. In Element, give the bot Moderator, or lower that permission under Room settings → Roles & Permissions. If its level is too low, the bot logs a warning at startup.

## Notes

- **Release retention.** By default every release is kept. `releases.keep_last: 10` keeps only the newest 10. `releases.max_repo_size: 2GB` removes the oldest releases while a repository (git data plus releases) is bigger than that, but always keeps the newest one. Releases outside these limits are never downloaded. Archived ones are deleted when a new release pushes them out, or when you tighten the limits. Raising the limits downloads them again. A release deleted upstream counts like any other, so it stays archived while it is among the newest. Git history is never removed.
- **First sync can take a while** for projects with many releases and large assets. While it runs, `list` shows progress (`🔄 archiving releases (3/10: v1.2)`). Use the retention limits above, `releases.max_asset_size`, or `download_assets: false` to limit this.
- **The bot's device is unverified,** so Element may show a shield warning on its messages. That is harmless.
- **Git LFS objects are not archived**; only the LFS pointer files are.

## Development

```sh
go test -tags goolm ./...                                                 # unit tests
go test -tags 'goolm integration' ./internal/archiver -run Integration -v # live test against real forges
CGO_ENABLED=0 go build -tags goolm -o archivist ./cmd/archivist
./archivist -config config.yaml
```

The build tag `goolm` is required: it selects the pure-Go Olm implementation.
