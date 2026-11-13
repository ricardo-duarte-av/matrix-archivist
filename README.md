# matrix-archivist

A Matrix bot that keeps a local archive of git repositories and their releases.
It lives in one room and takes commands from it, checks every repository for updates on a schedule,
and posts a daily summary of what changed.

- **Full git archive:** for every repository it keeps a bare clone with all branches and tags (`mirror.git`) plus a browsable checkout of the default branch (`checkout/`).
- **Releases:** archives release notes, raw metadata and downloadable assets from **GitHub**, **Gitea / Forgejo / Codeberg** and **GitLab**, including self-hosted instances, which are detected automatically.
- **End-to-end encryption:** works in encrypted rooms, with pure-Go crypto (no libolm or CGO).
- **Permissions:** commands that change things require a minimum room power level.
- **Web file browser:** a read-only listing of the archive. Download single files (resumable), or any folder as a zip.
- **Single static binary:** multi-arch Docker image (amd64/arm64).

## Commands

The base command is set by `command_prefix` (default `!archivist`).

| Command | Who | |
|---|---|---|
| `!archivist add <url> [url…]` | PL ≥ `min_power_level` | Add repositories. Each is cloned straight away and the bot replies when done. |
| `!archivist remove <#id\|url> [--purge]` | PL ≥ `min_power_level` | Stop tracking. Files stay on disk unless `--purge` is given. |
| `!archivist list` | everyone | Tracked repositories with their `#id` and state. |
| `!archivist refresh <#id\|url\|all>` | PL ≥ `min_power_level` | Check now and reply with what changed. |
| `!archivist status [#id\|url]` | everyone | Overall or per-repository status: errors, disk usage, release count. |
| `!archivist summary` | everyone | Post the summary of changes since the last scheduled summary. |
| `!archivist help` | everyone | Usage. |

Aliases: `ls`, `rm`/`del`, `update`/`pull`, `info`.

`#id` numbers are stable: removing #3 does not renumber #4.

Accepted URLs are `https://`, `http://`, `ssh://`, `git://` and `git@host:owner/repo`.
Local paths and `file://` are rejected.

## Storage layout

```
/data/
├── archivist.db            # repo list, archived releases, change history
├── crypto.db               # E2EE keys and Matrix state; back this up, don't delete it
└── repos/<host>/<owner>/<repo>/
    ├── mirror.git/         # bare clone: all branches + tags
    ├── checkout/           # working tree of the default branch
    └── releases/<tag>/
        ├── notes.md
        ├── release.json    # raw API response
        └── assets/…
```

To restore a repository from the archive, run `git clone /data/repos/github.com/owner/repo/mirror.git`.

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
| `releases.*` | Release archiving, asset downloads, size cap, prereleases. |
| `forges` | API tokens and self-hosted forges. A GitHub token is strongly recommended: without one the API allows 60 requests/hour. |
| `prune_deleted` | Whether branches and tags deleted upstream are deleted from the archive too. Default `false`: they are kept. |

## Failure alerts

When a repository fails `failure_alert_threshold` checks in a row (default 5), the bot posts an **@room** alert with the last error. When it works again, it posts a normal message saying so.

For @room to notify anyone, the bot needs the room's "Notify everyone" power level, which is 50 by default. In Element, give the bot Moderator, or lower that permission under Room settings → Roles & Permissions. If its level is too low, the bot logs a warning at startup.

## Notes

- **First sync can take a while** for projects with many releases and large assets (a CLI with 30 binaries × 300 releases, for example). Use `releases.max_asset_size` or `download_assets: false` to limit this.
- **Force-pushes upstream overwrite the archived branch.** The archive mirrors what upstream has, but with `prune_deleted: false` deleted branches and tags stay.
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
