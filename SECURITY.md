# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: **Security → Report a vulnerability** on this repository. Do not open a public issue.

Include what you found, how to reproduce it, and the version (`quadstack --version` or the release tag). You get an answer within a week. A confirmed issue is fixed in a new release and published as a GitHub security advisory, crediting you unless you prefer otherwise.

## Supported versions

Only the latest release gets fixes. Update with the installer; re-running it keeps your settings.

## What counts

Quadstack's guarantees are written down in [docs/GOVERNANCE.md](docs/GOVERNANCE.md). Anything that breaks one of them is a vulnerability, for example:

- signing in without being on the allow list, or a request that changes something without the session and `X-Quadstack` header;
- reading or writing outside the Quadlet directory, or through a symlink;
- an MCP grant reaching a stack, permission or time beyond what it allows, or a call that runs without an audit record;
- a secret value returned, logged or written to disk by Quadstack.

Out of scope: anything that needs the Quadlet account's own shell or files, since every signed-in operator has that account's full rights by design (see the non-goals in [docs/DESIGN.md](docs/DESIGN.md)); and vulnerabilities in Podman, systemd or your OIDC provider, which go to those projects.
