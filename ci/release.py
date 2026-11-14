#!/usr/bin/env python3
"""Select and publish tested main builds using GitHub's CLI; no third-party modules."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
ASSETS = ("quadstack_linux_amd64", "quadstack_linux_arm64", "SHA256SUMS")


def version_numbers(value):
    match = STABLE.fullmatch(value)
    if not match:
        raise ValueError("VERSION must be a stable version such as v0.7.0")
    return tuple(map(int, match.groups()))


def source_version():
    version = Path("VERSION").read_text().strip()
    version_numbers(version)
    return version


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def preview_version(version, sha):
    date = git("show", "-s", "--format=%cI", sha)
    # Git normalizes the committer timestamp to UTC for a deterministic tag.
    from datetime import datetime, timezone
    stamp = datetime.fromisoformat(date).astimezone(timezone.utc).strftime("%Y%m%d%H%M%S")
    return f"{version}-preview.{stamp}.g{sha[:12]}"


class GitHub:
    def __init__(self, repository):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid GitHub repository")
        self.repository = repository

    def api(self, endpoint):
        result = subprocess.run(
            ["gh", "api", f"repos/{self.repository}/{endpoint}"],
            text=True, capture_output=True, check=False,
        )
        if result.returncode:
            if "(HTTP 404)" in result.stderr:
                return None
            raise RuntimeError(result.stderr.strip() or "GitHub API request failed")
        return json.loads(result.stdout)

    def release(self, tag):
        return self.api(f"releases/tags/{tag}")

    def tag_commit(self, tag):
        ref = self.api(f"git/ref/tags/{tag}")
        if ref is None:
            return None
        obj = ref["object"]
        for _ in range(10):
            if obj["type"] == "commit":
                return obj["sha"]
            if obj["type"] != "tag":
                break
            obj = self.api(f"git/tags/{obj['sha']}")["object"]
        raise ValueError(f"{tag} does not resolve to a commit")

    def command(self, *args):
        subprocess.run(["gh", "release", *args, "--repo", self.repository], check=True)


def check_target(github, tag, sha, release):
    target = github.tag_commit(tag)
    if target is not None and target != sha:
        raise ValueError(f"{tag} already points to a different commit; tags are never moved")
    if release and target is None and release["target_commitish"] != sha:
        raise ValueError(f"the draft for {tag} targets a different commit")


def plan(github, version, sha, preview_only=False):
    numbers = version_numbers(version)
    if not preview_only:
        release = github.release(version)
        if release and github.tag_commit(version) == sha:
            if release["prerelease"]:
                raise ValueError("a stable tag is already used by a prerelease")
            return {"version": version, "channel": "stable", "published": not release["draft"]}
        latest = github.api("releases/latest")
        latest_numbers = version_numbers(latest["tag_name"]) if latest else None
        if latest_numbers is not None and numbers < latest_numbers:
            raise ValueError("VERSION is older than the latest stable release")
        if latest_numbers is None or numbers > latest_numbers:
            check_target(github, version, sha, release)
            return {"version": version, "channel": "stable", "published": False}
    tag = preview_version(version, sha)
    release = None if preview_only else github.release(tag)
    if release:
        check_target(github, tag, sha, release)
        if not release["prerelease"]:
            raise ValueError("a preview tag is already used by a stable release")
    return {"version": tag, "channel": "preview", "published": bool(release and not release["draft"])}


def asset_digests(directory):
    files = {name: directory / name for name in ASSETS}
    if any(not file.is_file() or file.stat().st_size == 0 for file in files.values()):
        raise ValueError("both binaries and SHA256SUMS must be present and nonempty")
    digests = {name: hashlib.sha256(file.read_bytes()).hexdigest() for name, file in files.items()}
    expected = "".join(f"{digests[name]}  {name}\n" for name in ASSETS[:2])
    if files["SHA256SUMS"].read_text() != expected:
        raise ValueError("SHA256SUMS does not match both tested binaries")
    return digests


def publish(github, version, channel, sha, directory):
    source = source_version()
    expected = source if channel == "stable" else preview_version(source, sha)
    if version != expected:
        raise ValueError("release version does not match the checked-out source")
    release = github.release(version)
    check_target(github, version, sha, release)
    if release and not release["draft"]:
        if release["prerelease"] != (channel == "preview"):
            raise ValueError("published release has the wrong channel")
        print(f"{version} is already published; nothing is replaced")
        return
    if channel == "stable":
        latest = github.api("releases/latest")
        if latest and version_numbers(latest["tag_name"]) > version_numbers(version):
            raise ValueError("refusing to replace a newer latest stable release")
    digests = asset_digests(directory)
    prerelease = channel == "preview"
    if release is None:
        args = ["create", version, "--draft", "--target", sha, "--title", version]
        if prerelease:
            args += ["--prerelease", "--latest=false", "--notes",
                     f"Preview of main at {sha}. Install with QS_VERSION=preview or pin this exact tag.\n\nThis is a preview build; stable installations do not select it."]
        else:
            args += ["--generate-notes"]
        github.command(*args)
    elif release["prerelease"] != prerelease:
        raise ValueError("existing draft has the wrong channel")
    # Clobber is confined to an unpublished draft, allowing interrupted uploads to resume.
    github.command("upload", version, *(str(directory / name) for name in ASSETS), "--clobber")
    uploaded = github.release(version)
    if not uploaded or not uploaded["draft"]:
        raise ValueError("the draft changed during publication; inspect it before retrying")
    check_target(github, version, sha, uploaded)
    assets = {asset["name"]: asset for asset in uploaded["assets"]}
    if set(assets) != set(ASSETS):
        raise ValueError("draft does not contain exactly the expected release assets")
    for name, digest in digests.items():
        asset = assets[name]
        if asset["state"] != "uploaded" or asset.get("digest") != f"sha256:{digest}":
            raise ValueError(f"uploaded asset {name} is incomplete or differs from the tested artifact")
    github.command("edit", version, "--draft=false", f"--prerelease={str(prerelease).lower()}",
                   f"--latest={str(not prerelease).lower()}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("plan", "publish"))
    parser.add_argument("--preview-only", action="store_true")
    parser.add_argument("--version")
    parser.add_argument("--channel", choices=("stable", "preview"))
    parser.add_argument("--directory", type=Path, default=Path("dist"))
    args = parser.parse_args()
    sha = git("rev-parse", "HEAD")
    if os.environ.get("GITHUB_SHA", sha) != sha:
        raise ValueError("checked-out commit differs from the workflow commit")
    github = GitHub(os.environ.get("GITHUB_REPOSITORY", "denMaier/quadstack"))
    if args.action == "plan":
        result = plan(github, source_version(), sha, args.preview_only)
        print(json.dumps(result))
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                for key, value in result.items():
                    output.write(f"{key}={str(value).lower() if isinstance(value, bool) else value}\n")
    else:
        if not args.version or not args.channel:
            parser.error("publish requires --version and --channel")
        publish(github, args.version, args.channel, sha, args.directory)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
