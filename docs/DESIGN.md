# How Quadstack works

Quadstack is one Go binary with the web UI embedded. It runs as a systemd user service of the account that owns the Quadlets and acts with exactly that account's rights. The rules it must always keep are in [GOVERNANCE.md](GOVERNANCE.md).

```
 browser ──OIDC session──┐          ┌── systemd user instance (D-Bus): status, start/stop, daemon-reload
                         ├─ quadstack ┼── Podman API (local Unix socket): containers, images, secrets, exec
 AI client ─bearer token─┘    │      ├── quadlet -dryrun: validation         (external programs, run as
                              │      ├── journalctl: logs                     the same account)
                              │      └── podlet: import (optional)
                              ├── ~/.config/containers/systemd   the Quadlet files, the only source of truth
                              └── ~/.local/state/quadstack        history, session key, MCP grants, audit log
```

## Non-goals

Homelab scale: one account, a few operators who all have the account's full rights. No teams, roles, tenants or per-user permissions in the web UI. No database, no own container runtime, no file format of its own, no git.

## Files and stacks

The Quadlet directory is read from scratch for every request (`Scan` in `stacks.go`); nothing about it is cached or stored elsewhere. A folder is a stack. At the top level, a `.pod` and the containers that set `Pod=` to it form a stack, and every other unit is a stack of its own. Top-level networks and volumes are shared resources. Symlinked files and folders are reported, not managed.

Every file access, from the web UI or MCP, opens each path component with `O_NOFOLLOW` relative to an open directory, so symlinks and concurrent renames cannot redirect it. Writes are atomic (temporary file and rename) and keep the file's mode. Before a file is changed, moved or deleted, its current content goes into `history/<path>/<unix nanoseconds>`. A move takes the history along. Up to 50 versions are kept per file. Deleted files are found by their history.

**Validation** copies every managed file into a scratch directory, puts the new content in place and runs Podman's `quadlet -dryrun -user` on it, so references between files resolve. Only messages about the edited file count. A timeout, a crash or a generator that cannot run fails the check. The verdict is Podman's alone; Quadstack only adds warnings the generator cannot give because they depend on other files or the disk (`hints`), and these never block a save. The one that looks at the disk, a missing `EnvironmentFile=`, is added only for signed-in operators (`EnvFileHints`), never for MCP.

## Actions

Every changing action (start, stop, restart, reload, delete, pull, prune, secrets) is an **operation** in `operations.go`: queued in memory, run one at a time, with a deadline, per-step state and output. The web API returns the operation at once and the UI follows it; MCP calls wait for it. A lost connection or a timeout is reported as `unknown` or `timed_out`, never as success, and nothing is retried automatically. Operations are lost on restart.

**Scheduled updates.** The web UI controls the Quadlet owner's existing `podman-auto-update.timer` over D-Bus. Enabling it persistently enables the timer, reloads systemd and starts it; disabling stops the timer, removes persistent and runtime enablement and reloads systemd. Each step is recorded in Activity, and a failed step stops the operation. The UI reads the actual enablement, running state, calendar schedule, next run and service result from systemd. There is no Quadstack scheduler or saved timer preference, and installing or starting Quadstack does not change the timer. The timer covers all eligible containers of the account, including ones Quadstack does not manage. Podman performs scheduled pulls and restarts outside Quadstack's operation queue, even while Quadstack is stopped. Stopping the timer does not cancel an update already running.

Stack auto-update participation is On, Off or Mixed according to deployed containers' `io.containers.autoupdate` labels. A missing container or unavailable Podman makes it Unknown; stacks without container workloads are not applicable. The labels reflect the deployed Quadlet settings, including drop-ins: change `AutoUpdate=` in the ordinary files and deploy to change participation. New container templates omit it, leaving participation off. There are no per-stack timers, bulk policy edits or MCP timer controls.

**Shared network namespaces.** `Scan` records which container joins another's network namespace and which Quadlet defines that container (`linkNetns`). Every unit action is planned around these edges (`planUnits`): dependents are stopped before the container they depend on is stopped or restarted and started again after a restart, and on a start that container comes first. Only dependents that are part of the action or running now are included. Quadstack sends each step itself instead of relying on `Requires=`, which `Network=container:<name>` does not add.

## Sign-in

OpenID Connect with PKCE, state and nonce, in `auth.go`. After the callback the claims are checked against `QUADSTACK_ALLOWED_USERS` and `QUADSTACK_ALLOWED_GROUPS`, and the session is a cookie signed with HMAC-SHA256 with a key in the state directory. It lasts 12 hours. Changing requests also need the header `X-Quadstack: 1`; websockets only accept the public URL as origin.

## AI access (MCP)

`/mcp` is a stateless streamable-HTTP MCP endpoint (`mcp.go`, `mcptools.go`). A bearer token selects a **grant**: an end time and rules (permission → stacks). For each request an MCP server is built that lists only the tools the grant allows. Each call:

1. is written to the audit log as `started`, and refused if that fails;
2. reads the grant again, so an ended or changed grant applies at once;
3. works out the stack of every file, unit and container it touches, before and after the change, and checks the permission for each;
4. is checked once more when its operation leaves the queue;
5. is written to the audit log with its result.

## Deployment

`deploy/install.sh` installs the release binary for an account, checks it against `SHA256SUMS`, writes `~/.config/quadstack/quadstack.env` and the user unit, and enables lingering and `podman.socket`. On Proxmox it creates an LXC with the community-scripts engine first. `make dev` builds the same setup in Docker with Dex as the identity provider.

## Builds and releases

One CI workflow checks pull requests and main, builds the frontend once, and builds and smoke-tests static Linux amd64 and arm64 binaries on native runners. A disposable Linux account runs the systemd/Podman integration test. The required `test` check succeeds only after checks and both architecture builds succeed. Pull requests publish no releases and have read-only tokens.

Successful main builds publish installable GitHub Releases. If `VERSION` is newer than the latest stable release, the workflow publishes that stable version; otherwise it publishes an immutable prerelease named `vX.Y.Z-preview.COMMITDATE.gSHA`. A new version in a merged PR is the release decision. Main runs are queued, so publication cannot race. Manual runs on main recover failures. Re-running a published commit is a no-op; published tags and assets are never replaced. Reserved tags must resolve to the built commit. Drafts can be resumed, and become public only after both binaries, checksums and provenance are complete. Publication uses the tested artifacts with a separate write-capable job, without a personal access token or a second tag-triggered workflow.

The installer accepts `QS_VERSION=stable`, `preview` or an exact release tag. Stable is the default for a new install; an installed preview defaults to the preview channel on later updates. Interactive updates can switch channels, including back to stable. Preview resolution selects only published Quadstack preview prereleases; stable resolution uses GitHub's latest stable release. Both resolve once to an immutable tag before fetching the binary and matching service file, with checksum verification for either channel. No arbitrary branch builds or device deployment are performed by CI.
