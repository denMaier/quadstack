# Quadstack

A small web UI for **rootless Podman Quadlets**, in the spirit of [Dockge](https://github.com/louislam/dockge): your `.container`, `.pod`, `.network` and other Quadlet files grouped into stacks you can edit, check, start, stop and update from the browser, with live logs and container shells.

Quadstack runs as a user service of the account that owns the Quadlets. It has no root helper and needs no Podman socket. It only edits the files in `~/.config/containers/systemd` and talks to that user's systemd, so everything it does stays ordinary Quadlet configuration.

- **Stacks.**
  - Each folder in the Quadlet directory is a stack.
  - At the top level, a `.pod` forms a stack together with the containers that set `Pod=` to it. Every other unit is a stack of its own.
  - Drop-ins (`name.container.d/*.conf`) and `.env` files are listed with their stack.
- **Editor.** CodeMirror, with completion of Quadlet keys and sections.
  - **Check** runs Podman's own generator (`quadlet -dryrun -user`) on a scratch copy of the whole directory, so references between files resolve. A file Podman rejects is not saved unless you insist.
  - **Show generated unit** shows the systemd service Quadlet will produce.
  - **Save & reload** runs `daemon-reload`. **Deploy** also restarts the unit.
- **History, without git.** Every save, move or delete first keeps the previous version under `~/.local/state/quadstack/history`, for the last 50 versions per file. You can diff any version against the editor and load it back.
- **Units.** Start, stop or restart a whole stack or a single unit. You see their systemd state and whether they start at boot (`[Install]`).
- **Logs.** A live `journalctl` view per stack or per unit.
- **Update.** Pulls the images of a stack's containers in a live terminal and offers a restart afterwards.
- **Shell.** Runs `podman exec` into a running container in the browser. You can turn it off with `QUADSTACK_TERMINAL=false`.
- **Networks & volumes.** Top-level `.network` and `.volume` files are shared resources rather than stacks, listed with the stacks that use them, next to the Podman networks and volumes no file defines. Each stack shows its containers' bind mounts, volumes, networks and secrets.
- **Secrets.** Podman secrets: create, replace and delete, with the files that use each one. Values go in over stdin and are never shown. In a container's editor, **Secrets** moves an `Environment=` value or a mounted key file into a secret and rewrites the line to `Secret=`.
- **Images.** A list with sizes and usage, plus pruning of dangling or unused images.
- **Import.** Paste a `docker run` command or a compose file and get Quadlet files. This needs [podlet](https://github.com/containers/podlet) on `PATH`. The installer offers to install it to `/usr/local/bin` and keeps it up to date on updates. Unattended runs skip it unless `QS_PODLET=1` is set.
- **AI access over MCP, off by default.** See [AI access](#ai-access-mcp).
- **Sign-in with OpenID Connect only** (Pocket ID, Authentik, Keycloak, …). Access is limited to the listed users and/or groups. Quadstack refuses to start without such a list.

## Requirements

- Linux with systemd and Podman 4.4 or newer. Podman 5 is recommended.
- A user account that owns the Quadlets, with lingering enabled (`loginctl enable-linger <user>`).
- An OIDC client whose redirect URI is `https://<your host>/auth/callback`.
- A reverse proxy with TLS in front of Quadstack.

## Install

Run the installer as root. It works on a Proxmox VE host and on an existing machine:

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/install.sh)"
```

It does different things depending on where it runs:

- **On a Proxmox VE host**, it creates a new LXC. It uses the [community-scripts](https://github.com/community-scripts/core) engine, pinned to a tested commit, with its usual dialogs for resources, storage and network. The container is a Debian 13 LXC, unprivileged and with nesting and keyctl enabled, which rootless Podman needs. Inside it, the installer sets up Podman and an account `quadlets` that owns the Quadlets. Later, `update` in the container installs the latest release.
- **Anywhere else** (an existing LXC, VM or server with systemd), it installs Quadstack on that machine as an add-on. It lists the accounts that already have a Quadlet directory and lets you choose one or create a new one. Podman is installed with apt if it's missing. Running it again offers **update** or **reconfigure**.

It asks for:

- the public URL;
- the OIDC issuer, client ID and secret;
- the groups and/or users allowed to sign in;
- the listen address;
- whether to allow shells into containers;
- the longest an MCP grant may last.

It shows the redirect URI to register with your provider before asking for the client. The binary is checked against the release's `SHA256SUMS`. When the account is a system account, the installer offers the journal ACL from [Logs of system accounts](#logs-of-system-accounts).

For an unattended run, answer the questions in the environment and set `QS_YES=1` (see the top of [`deploy/install.sh`](deploy/install.sh)):

```sh
QS_YES=1 QS_USER=quadlets QS_PUBLIC_URL=https://quadstack.example.com \
QS_ISSUER=https://id.example.com QS_CLIENT_ID=quadstack QS_CLIENT_SECRET=… QS_ALLOWED_GROUPS=admins \
  bash -c "$(curl -fsSL https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/install.sh)"
```

In an unprivileged LXC, the account's subordinate IDs have to fit in the container's 65536 IDs. A new account therefore gets the IDs above its own UID, which leaves containers about 64,500 UIDs instead of 65,536. Images with files owned by higher IDs, such as `nobody` (65534), fail to pull there. Give the LXC a larger ID map if you need those.

### Manual install

As the account that owns the Quadlets:

```sh
mkdir -p ~/.local/bin ~/.config/quadstack ~/.config/systemd/user
curl -L -o ~/.local/bin/quadstack \
  https://github.com/denMaier/quadstack/releases/latest/download/quadstack_linux_amd64
chmod +x ~/.local/bin/quadstack

curl -L -o ~/.config/quadstack/quadstack.env \
  https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/quadstack.env.example
chmod 600 ~/.config/quadstack/quadstack.env
$EDITOR ~/.config/quadstack/quadstack.env

curl -L -o ~/.config/systemd/user/quadstack.service \
  https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/quadstack.service
systemctl --user daemon-reload
systemctl --user enable --now quadstack
```

Use `quadstack_linux_arm64` on ARM. For a system account without a login shell, run these with `sudo -iu <user>`. If `systemctl --user` can't find the bus, use `sudo machinectl shell <user>@`.

### Logs of system accounts

journald gives only accounts with UID 1000 or higher their own journal. When the Quadlets belong to a system account (UID below 1000, as created by `useradd --system` or by some managers), its logs land in the system journal, which it may not read, and Quadstack shows a warning instead of logs. Grant the account read access with ACLs, as root (replace `quadlets` with the account name):

```sh
printf '%s\n' \
  'a+ /var/log/journal    - - - - d:user:quadlets:r-x,user:quadlets:r-x' \
  'A+ /var/log/journal/%m - - - - d:user:quadlets:r-x,user:quadlets:r-x' \
  > /etc/tmpfiles.d/quadstack-journal.conf
systemd-tmpfiles --create /etc/tmpfiles.d/quadstack-journal.conf
```

This lets the account, and so anyone who can sign in to Quadstack, read the whole system journal. Adding the account to the `systemd-journal` group does the same, but only takes effect after its systemd user instance, and with it every container, restarts.

### Update

Run the installer again and choose **update**, or run `update` in an LXC it created. By hand, replace the binary and restart; settings and history stay:

```sh
curl -L -o ~/.local/bin/quadstack \
  https://github.com/denMaier/quadstack/releases/latest/download/quadstack_linux_amd64
systemctl --user restart quadstack
~/.local/bin/quadstack -version
```

## Configuration

Every setting is an environment variable. See [`deploy/quadstack.env.example`](deploy/quadstack.env.example).

| Variable | Default | |
| --- | --- | --- |
| `QUADSTACK_LISTEN` | `127.0.0.1:8090` | Address to listen on |
| `QUADSTACK_PUBLIC_URL` | *required* | The URL people open, used for the OIDC callback |
| `QUADSTACK_OIDC_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET` | *required* (secret optional for public clients) | |
| `QUADSTACK_OIDC_SCOPES` | `openid profile email groups` | |
| `QUADSTACK_OIDC_GROUPS_CLAIM` | `groups` | Read from the ID token, or from userinfo when the ID token has none |
| `QUADSTACK_ALLOWED_USERS` | | `sub`, `email` or `preferred_username` values, comma separated |
| `QUADSTACK_ALLOWED_GROUPS` | | Groups that may sign in |
| `QUADSTACK_QUADLET_DIR` | `~/.config/containers/systemd` | |
| `QUADSTACK_STATE_DIR` | `~/.local/state/quadstack` | History and the session key |
| `QUADSTACK_QUADLET_BIN` | found in `/usr/libexec/podman` or `/usr/lib/podman` | Validation is skipped when it is missing |
| `QUADSTACK_PODLET_BIN` | `podlet` on `PATH` | Enables Import |
| `QUADSTACK_TERMINAL` | `true` | Shells into containers, and the MCP `exec` tool |
| `QUADSTACK_MCP_MAX_HOURS` | `24` | The longest an MCP grant may last; `0` turns MCP access off |

## AI access (MCP)

Quadstack has an [MCP](https://modelcontextprotocol.io) endpoint at `<QUADSTACK_PUBLIC_URL>/mcp` for AI assistants such as Claude Code. It refuses every request until you create a **grant** under **AI access**. A grant has:

- **An end.** It lasts from 15 minutes up to `QUADSTACK_MCP_MAX_HOURS` (24 by default), and **End now** stops it at once. An edit can move the end, but never past that maximum from now.
- **Its own bearer token.** The token is shown once. Quadstack stores only its SHA-256 hash.
  - The token outlives the grant's end: **Reactivate** an ended grant and the same client is let back in for the time you choose. So each machine is set up once.
  - **Delete** removes the grant and its token for good, for example when you suspect the token was copied.
- **Rules.** A table of permissions by stack, plus some account-wide permissions. You can start from a preset (Nothing, Read only, Read & operate, Everything) and then change single boxes. Clicking a column or row name switches the whole line.

### Connecting a client

After **Create grant**, the page shows the token with a ready command for Claude Code:

```sh
claude mcp add --scope user --transport http quadstack-<host> https://quadstack.example.com/mcp \
  --header "Authorization: Bearer qsm_…"
```

You do this once per machine, because reactivating a grant keeps its token. For a new token, run `claude mcp remove quadstack-<host>` and add the server again. The page also shows the settings as JSON for other clients. Claude Code stores the token in `~/.claude.json`, which is one more reason to delete a grant when a device is lost.

The server is named after the machine (`quadstack-<hostname>`), so you can add several Quadstack machines side by side. Each keeps its own grants, end times and audit log. Claude Code prefixes the tools with the server name, for example `mcp__quadstack-servarr__list_stacks`.

The transport is streamable HTTP without sessions. If a reverse proxy is in front, it must pass the `Authorization` header through to `/mcp`. While a grant is ended, its client gets `401`. After **Reactivate**, Claude Code reconnects with the same token, either on its own or through `/mcp` → reconnect.

**Where calls come from.** Each grant shows the address of its last call, and the audit log records the address of every call. Behind a reverse proxy this reads `<client> via <proxy>`, where the client address comes from `X-Forwarded-For`. An unfamiliar address during one of your grants is the sign of a copied token. Make the token useless outside your network by letting Zoraxy accept `/mcp` only from your LAN or VPN.

### Permissions

**Scopes.** A per-stack permission applies to the stacks you tick. Stacks are named `d/<folder>` for folders and `r/<name>` for top-level units. Two scopes are special:

- **All stacks** (`*`) covers every stack, including stacks created later, and the shared networks and volumes.
- **Shared networks & volumes** (`shared`) covers the top-level `.network` and `.volume` files and their drop-ins.

A new file in a new folder belongs to a stack that does not exist yet, so creating it needs Write on **All stacks**.

| Permission | Column | Tools | Allows |
|---|---|---|---|
| `stacks.read` | See | `list_stacks` | Stacks with their files, unit state and containers; with `shared`, also the shared files |
| `files.read` | Read | `read_file` | Quadlet and drop-in files |
| `env.read` | Env | with `read_file`, `file_history` | `.env` files, which often hold credentials. Needs Read or History as well |
| `history.read` | History | `file_history` | The saved versions of a file |
| `files.write` | Write | `write_file`, `validate_file`, `daemon_reload` | Creating and replacing files. A file Podman's `quadlet` rejects is not saved, and MCP has no override |
| `files.delete` | Delete | `delete_file` | Deleting files. The previous version stays in the history |
| `logs.read` | Logs | `read_logs` | Journal lines of a unit or a whole stack, up to 2000 |
| `units.start`, `units.stop`, `units.restart` | Start, Stop, Restart | `unit_action`, `stack_action` | Each action separately. Restart is also needed for `write_file` with `apply: restart`, and Stop for `delete_file` with `stop` |
| `images.pull` | Pull | `pull_images` | Pulling newer images of the stack's containers |
| `exec` | Exec | `exec` | Running a command in the stack's containers, with no terminal and a timeout of up to 10 minutes. Only offered while `QUADSTACK_TERMINAL=true` |
| `resources.read` | account | `list_resources` | Podman networks and volumes, and the Quadlet network and volume files |
| `images.read` | account | `list_images` | Local images |
| `images.prune` | account | `prune_images` | Removing dangling or unused images |
| `secrets.read` | account | `list_secrets` | Secret names and where they are used. Values are never returned |
| `secrets.write` | account | `set_secret` | Creating secrets and replacing their values |
| `secrets.delete` | account | `remove_secret` | Deleting secrets |

`access_info` is always offered. It tells the assistant what its grant allows and when it ends.

### How the rules are enforced

- **Hidden tools.** Tools that the grant does not allow are not listed, and calling one fails.
- **Live rules.** The MCP server is rebuilt for every request from the grant as it is now. Each call checks the grant again before it runs, so an edit, an end or a revocation applies to a session that is already running.
- **A file's stack before and after a change.** A change must be allowed for the stack the file is in and for the stack it ends up in. A top-level container that sets `Pod=` joins that pod's stack, so this edit needs Write on both stacks.
- **Units and containers.** Units and containers are matched to the stack whose file defines them. A container that no Quadlet file started needs Exec on **All stacks**.

### Audit log

Every call goes to `~/.local/state/quadstack/mcp-audit.jsonl` and to the **AI access** page, with the grant, the caller's address, the tool, the arguments and the result. The result is `ok`, `denied` or `error`. File contents are replaced by their size and a hash, and secret values are left out. The log rotates to `mcp-audit.jsonl.1` at 5 MB. Grants are kept in `mcp-grants.json` in the same directory until you delete them.

## Security notes

- Anyone who can sign in can do what the Quadlet account can do. That includes mounting any path the account can read into a container. Keep the allowed users and groups short.
- Changing requests need a same-site session cookie and a custom header. Websockets only accept the origin of `QUADSTACK_PUBLIC_URL`.
- An MCP grant acts with the account's rights within its rules. Exec and Write on a stack are effectively full control of that stack's containers; Write can also bind-mount any path the account can read.
- Quadstack is meant for a LAN or VPN. Don't expose it to the internet without more protection in front.

## Coming from another manager

Quadstack works on any existing Quadlet directory and doesn't need to adopt anything. If another tool generated the files and keeps its own database (Quadletman, for example), stop using that tool for editing once you switch, or the two will disagree. Never let it delete the account or "compartment" that owns the files.

## Development

```sh
make test          # go vet, go test, svelte-check
make build         # builds the web UI and the binary
cd web && npm run dev   # Vite on :5173, proxying to a backend on :8090
```

To release, push a tag `vX.Y.Z`. GitHub Actions runs the tests and publishes `quadstack_linux_amd64`, `quadstack_linux_arm64` and `SHA256SUMS`, with the tag built in as the version.

The backend is Go: go-oidc, go-systemd (D-Bus), coder/websocket and creack/pty. The UI is Svelte 5 with CodeMirror 6 and xterm.js, embedded in the binary.

## License

AGPL-3.0
