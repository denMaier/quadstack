import copy
import hashlib
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import release


SHA = "1234567890abcdef" * 2 + "12345678"
OTHER_SHA = "abcdef0123456789" * 2 + "abcdef01"
PREVIEW = "v0.7.0-preview.20261114154004.g1234567890ab"


def record(sha=SHA, draft=False, prerelease=False):
    return {"target_commitish": sha, "draft": draft, "prerelease": prerelease, "assets": []}


class FakeGitHub:
    def __init__(self):
        self.latest = None
        self.releases = {}
        self.tags = {}
        self.commands = []
        self.fail_upload = False
        self.corrupt_upload = False

    def api(self, endpoint):
        if endpoint != "releases/latest":
            raise AssertionError(endpoint)
        return self.latest

    def release(self, tag):
        return copy.deepcopy(self.releases.get(tag))

    def tag_commit(self, tag):
        return self.tags.get(tag)

    def command(self, *args):
        self.commands.append(args)
        action, tag = args[:2]
        if action == "create":
            self.releases[tag] = record(draft=True, prerelease="--prerelease" in args)
        elif action == "upload":
            if self.fail_upload:
                raise subprocess.CalledProcessError(1, "gh release upload")
            self.releases[tag]["assets"] = [
                {"name": Path(path).name, "state": "uploaded",
                 "digest": "sha256:" + hashlib.sha256(Path(path).read_bytes()).hexdigest()}
                for path in args[2:] if not path.startswith("--")
            ]
            if self.corrupt_upload:
                self.releases[tag]["assets"][0]["digest"] = "sha256:bad"
        elif action == "edit":
            self.releases[tag]["draft"] = False
            self.tags[tag] = SHA
        else:
            raise AssertionError(args)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.github = FakeGitHub()
        self.preview = patch("release.preview_version", return_value=PREVIEW)
        self.preview.start()
        self.addCleanup(self.preview.stop)
        source = patch("release.source_version", return_value="v0.7.0")
        source.start()
        self.addCleanup(source.stop)

    def test_new_version_becomes_stable(self):
        for latest in (None, {"tag_name": "v0.6.1"}):
            self.github.latest = latest
            self.assertEqual(release.plan(self.github, "v0.7.0", SHA),
                             {"version": "v0.7.0", "channel": "stable", "published": False})

    def test_normal_merge_becomes_preview(self):
        self.github.latest = {"tag_name": "v0.7.0"}
        self.github.releases["v0.7.0"] = record(sha=OTHER_SHA)
        self.github.tags["v0.7.0"] = OTHER_SHA
        self.assertEqual(release.plan(self.github, "v0.7.0", SHA),
                         {"version": PREVIEW, "channel": "preview", "published": False})

    def test_pull_request_needs_no_release_access(self):
        with patch.object(self.github, "api", side_effect=AssertionError("unexpected API read")), \
                patch.object(self.github, "release", side_effect=AssertionError("unexpected release read")):
            self.assertEqual(release.plan(self.github, "v0.7.0", SHA, preview_only=True)["channel"], "preview")

    def test_rerun_of_published_commit_is_noop_even_after_a_newer_release(self):
        self.github.latest = {"tag_name": "v0.8.0"}
        self.github.releases["v0.7.0"] = record()
        self.github.tags["v0.7.0"] = SHA
        self.assertTrue(release.plan(self.github, "v0.7.0", SHA)["published"])
        release.publish(self.github, "v0.7.0", "stable", SHA, Path("missing"))
        self.assertEqual(self.github.commands, [])

    def test_release_version_cannot_go_backwards(self):
        self.github.latest = {"tag_name": "v0.8.0"}
        with self.assertRaisesRegex(ValueError, "older"):
            release.plan(self.github, "v0.7.0", SHA)

    def test_reserved_tag_cannot_be_reused_for_another_commit(self):
        self.github.tags["v0.7.0"] = OTHER_SHA
        with self.assertRaisesRegex(ValueError, "different commit"):
            release.plan(self.github, "v0.7.0", SHA)

    def test_draft_must_target_the_built_commit(self):
        self.github.releases["v0.7.0"] = record(sha=OTHER_SHA, draft=True)
        with self.assertRaisesRegex(ValueError, "different commit"):
            release.plan(self.github, "v0.7.0", SHA)

    def test_stable_version_validation(self):
        for version in ("", "v01.2.3", "1.2.3", "v1.2.3-rc.1", "v1.2.3\nwrite=true"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.version_numbers(version)

    def artifacts(self, directory):
        for name in release.ASSETS[:2]:
            (directory / name).write_bytes(name.encode())
        (directory / "SHA256SUMS").write_text("".join(
            f"{hashlib.sha256((directory / name).read_bytes()).hexdigest()}  {name}\n"
            for name in release.ASSETS[:2]
        ))

    def test_publish_stages_and_verifies_assets_before_making_release_public(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.artifacts(directory)
            release.publish(self.github, "v0.7.0", "stable", SHA, directory)
            self.assertEqual([args[0] for args in self.github.commands], ["create", "upload", "edit"])
            self.assertIn("--draft", self.github.commands[0])
            self.assertIn("--latest=true", self.github.commands[-1])
            self.assertFalse(self.github.releases["v0.7.0"]["draft"])

    def test_interrupted_draft_can_resume(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.artifacts(directory)
            self.github.fail_upload = True
            with self.assertRaises(subprocess.CalledProcessError):
                release.publish(self.github, "v0.7.0", "stable", SHA, directory)
            self.assertTrue(self.github.releases["v0.7.0"]["draft"])
            self.github.fail_upload = False
            self.github.commands.clear()
            release.publish(self.github, "v0.7.0", "stable", SHA, directory)
            self.assertEqual([args[0] for args in self.github.commands], ["upload", "edit"])

    def test_bad_assets_never_publish(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.artifacts(directory)
            (directory / "quadstack_linux_arm64").write_bytes(b"corrupt")
            with self.assertRaisesRegex(ValueError, "SHA256SUMS"):
                release.publish(self.github, "v0.7.0", "stable", SHA, directory)
            self.assertEqual(self.github.commands, [])

    def test_corrupted_upload_remains_a_draft(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.artifacts(directory)
            self.github.corrupt_upload = True
            with self.assertRaisesRegex(ValueError, "differs"):
                release.publish(self.github, "v0.7.0", "stable", SHA, directory)
            self.assertTrue(self.github.releases["v0.7.0"]["draft"])
            self.assertNotIn("edit", [args[0] for args in self.github.commands])

    def test_preview_never_becomes_latest_stable(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.artifacts(directory)
            release.publish(self.github, PREVIEW, "preview", SHA, directory)
            self.assertIn("--prerelease", self.github.commands[0])
            self.assertIn("--latest=false", self.github.commands[-1])
            self.assertIn("--prerelease=true", self.github.commands[-1])


class GitHubTests(unittest.TestCase):
    def test_only_404_is_treated_as_absent(self):
        github = release.GitHub("owner/repo")
        for status in (401, 403, 429, 500):
            result = subprocess.CompletedProcess([], 1, "", f"gh: failed (HTTP {status})")
            with patch("subprocess.run", return_value=result), self.assertRaises(RuntimeError):
                github.api("releases/latest")
        result = subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)")
        with patch("subprocess.run", return_value=result):
            self.assertIsNone(github.api("releases/latest"))

    def test_annotated_tag_is_resolved_to_a_commit(self):
        github = release.GitHub("owner/repo")
        with patch.object(github, "api", side_effect=[
            {"object": {"type": "tag", "sha": OTHER_SHA}},
            {"object": {"type": "commit", "sha": SHA}},
        ]):
            self.assertEqual(github.tag_commit("v0.7.0"), SHA)

    def test_preview_identity_uses_commit_time_in_utc(self):
        with patch("release.git", return_value="2026-11-14T16:40:04+01:00"):
            self.assertEqual(release.preview_version("v0.7.0", SHA), PREVIEW)


if __name__ == "__main__":
    unittest.main()
