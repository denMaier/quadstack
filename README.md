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
- **Images.** A list with sizes and usage, plus pruning of dangling or unused images.
- **Import.** Paste a `docker run` command or a compose file and get Quadlet files. This needs [podlet](https://github.com/containers/podlet) on `PATH`.
- **Sign-in with OpenID Connect only** (Pocket ID, Authentik, Keycloak, …). Access is limited to the listed users and/or groups. Quadstack refuses to start without such a list.

## Requirements

- Linux with systemd and Podman 4.4 or newer. Podman 5 is recommended.
- A user account that owns the Quadlets, with lingering enabled (`loginctl enable-linger <user>`).
- An OIDC client whose redirect URI is `https://<your host>/auth/callback`.
- A reverse proxy with TLS in front of Quadstack.

## Install

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

For a system account without a login shell, run these with `sudo -iu <user>`. If `systemctl --user` can't find the bus, use `sudo machinectl shell <user>@`.

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
| `QUADSTACK_TERMINAL` | `true` | Shells into containers |

## Security notes

- Anyone who can sign in can do what the Quadlet account can do. That includes mounting any path the account can read into a container. Keep the allowed users and groups short.
- Changing requests need a same-site session cookie and a custom header. Websockets only accept the origin of `QUADSTACK_PUBLIC_URL`.
- Quadstack is meant for a LAN or VPN. Don't expose it to the internet without more protection in front.

## Coming from another manager

Quadstack works on any existing Quadlet directory and doesn't need to adopt anything. If another tool generated the files and keeps its own database (Quadletman, for example), stop using that tool for editing once you switch, or the two will disagree. Never let it delete the account or "compartment" that owns the files.

## Development

```sh
make test          # go vet, go test, svelte-check
make build         # builds the web UI and the binary
cd web && npm run dev   # Vite on :5173, proxying to a backend on :8090
```

The backend is Go: go-oidc, go-systemd (D-Bus), coder/websocket and creack/pty. The UI is Svelte 5 with CodeMirror 6 and xterm.js, embedded in the binary.

## License

AGPL-3.0
