# Contributing

Issues and pull requests are welcome.

## Before you open a pull request

[Development](README.md#development) in the README describes `make dev`, and [docs/DESIGN.md](docs/DESIGN.md) and [docs/GOVERNANCE.md](docs/GOVERNANCE.md) describe how Quadstack works and what it must always guarantee. Read the governance rules before changing anything around access, the installer or releases.

Run the checks CI runs:

```sh
make web
make test
bash dev/test-systemd.sh   # real systemd and rootless Podman, in a disposable account
```

The last one needs a Linux host with systemd, Podman and passwordless `sudo`; CI runs it on every pull request if you don't have one.

Keep a pull request to one change. This project keeps its dependencies few, so a new one needs a good reason in the description.

## Pull request titles

Pull requests are squash-merged, so the title becomes the commit on `main`. Titles follow [Conventional Commits](https://www.conventionalcommits.org/), and a check enforces it:

| Title | Release |
|---|---|
| `fix: handle an empty config` | patch (0.7.0 → 0.7.1) |
| `feat: add a dark theme` | minor (0.7.0 → 0.8.0) |
| `feat!: drop the old config format` | breaking: minor while below 1.0, major after |
| `docs:`, `test:`, `refactor:`, `ci:`, `chore:` | none |

A scope is optional: `fix(installer): …`. Mark a breaking change with `!` after the type. A change to how access is decided is at least a minor version.

## Releases

Releases are automatic. Every main build publishes a preview. [release-please](https://github.com/googleapis/release-please) keeps a pull request open that bumps `VERSION` and the changelog; merging it makes CI build, verify and publish the stable release. Dependabot keeps dependencies current, and a release made only of dependency fixes goes out on its own.

Don't edit `VERSION` by hand; the release pull request updates it.

## License

By contributing you agree that your contribution is licensed under the project's license, AGPL-3.0 ([LICENSE](LICENSE)).
