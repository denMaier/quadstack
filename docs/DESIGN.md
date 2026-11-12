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

**Validation** copies every managed file into a scratch directory, puts the new content in place and runs Podman's `quadlet -dryrun -user` on it, so references between files resolve. Only messages about the edited file count. A timeout, a crash or a generator that cannot run fails the check.

## Actions

Every changing action (start, stop, restart, reload, delete, pull, prune, secrets) is an **operation** in `operations.go`: queued in memory, run one at a time, with a deadline, per-step state and output. The web API returns the operation at once and the UI follows it; MCP calls wait for it. A lost connection or a timeout is reported as `unknown` or `timed_out`, never as success, and nothing is retried automatically. Operations are lost on restart.

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
