"""在临时 Git 仓库验证版本规则，不修改当前仓库的提交和 tag。"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("git-version.sh").resolve()


class GitVersionTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.repo = Path(self.directory.name)
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Version Test")
        self.git("config", "user.email", "version@example.invalid")
        self.commit("2026-01-01T00:00:00+00:00")

    def git(self, *args, date=None):
        env = os.environ.copy()
        if date:
            env.update(GIT_AUTHOR_DATE=date, GIT_COMMITTER_DATE=date)
        return subprocess.check_output(
            ["git", *args], cwd=self.repo, env=env, text=True
        ).strip()

    def commit(self, date):
        self.git("commit", "-q", "--allow-empty", "-m", date, date=date)

    def version(self):
        return subprocess.check_output(
            ["bash", str(SCRIPT)], cwd=self.repo, text=True
        ).strip()

    def assert_commit_version(self):
        self.assertEqual(self.version(), self.git("rev-parse", "--short=12", "HEAD"))

    def test_without_tag(self):
        self.assert_commit_version()

    def test_lightweight_tag_at_head(self):
        self.git("tag", "v1.0.0")
        self.assertEqual(self.version(), "v1.0.0")

    def test_annotated_tag_at_head(self):
        self.git("tag", "-a", "v1.0.0", "-m", "release")
        self.assertEqual(self.version(), "v1.0.0")

    def test_newer_commit(self):
        self.git("tag", "v1.0.0")
        self.commit("2026-01-02T00:00:00+00:00")
        self.assert_commit_version()

    def test_annotated_tag_created_after_newer_commit(self):
        first_commit = self.git("rev-parse", "HEAD")
        self.commit("2026-01-02T00:00:00+00:00")
        self.git("tag", "-a", "v1.0.0", first_commit, "-m", "late tag")
        self.assert_commit_version()

    def test_same_commit_time_keeps_tag(self):
        self.git("tag", "v1.0.0")
        self.commit("2026-01-01T00:00:00+00:00")
        self.assertEqual(self.version(), "v1.0.0")

    def test_older_commit_time_keeps_tag(self):
        self.git("tag", "v1.0.0")
        self.commit("2025-12-31T00:00:00+00:00")
        self.assertEqual(self.version(), "v1.0.0")

    def test_unreachable_tag_is_ignored(self):
        self.git("checkout", "-q", "-b", "other")
        self.commit("2026-01-03T00:00:00+00:00")
        self.git("tag", "v9.0.0")
        self.git("checkout", "-q", "main")
        self.assert_commit_version()

    def test_nearest_tag_is_used(self):
        self.git("tag", "v1.0.0")
        self.commit("2026-01-02T00:00:00+00:00")
        self.git("tag", "v2.0.0")
        self.assertEqual(self.version(), "v2.0.0")


if __name__ == "__main__":
    unittest.main()
