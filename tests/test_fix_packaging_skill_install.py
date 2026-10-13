"""
skill-duplicate: scripts/install_source.py installs the agent skill once.

It used to copy skills/makewand-orchestrator to both makewand-orchestrator and
trio-orchestrator, two directories declaring the same `name:`, so agents saw the
skill twice. Existing user directories are never deleted. An old
trio-orchestrator copy is kept current while its front matter still declares
the makewand-orchestrator name; only the leading `---` block is read, so a
user's own skill that merely quotes that name in its body is never touched.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import importlib.util
import io
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parent.parent
SKILL_TEXT = "---\nname: makewand-orchestrator\ndescription: test skill\n---\n\n# Skill\n"
OLD_SKILL_TEXT = "---\nname: makewand-orchestrator\ndescription: instructions of an older release\n---\n\n# Old\n"
# A user's own skill whose body shows another skill's front matter.
USER_SKILL_TEXT = ("---\nname: trio-orchestrator\ndescription: my own skill\n---\n\n"
                   "Example front matter:\n\n```yaml\nname: makewand-orchestrator\n```\n")


def _load_installer():
    spec = importlib.util.spec_from_file_location("mw_install_source_under_test", REPO_ROOT / "scripts" / "install_source.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _fake_check_output(args, **_kwargs):
    if str(args[0]).endswith("makewand-server"):
        return "makewand version 1.2.3\n"
    return "makewand 1.2.3\n"


def _fake_run(args, **_kwargs):
    # `go build ... -o <native>` produces the native engine; smoke runs succeed.
    if args[0] == "go":
        native = Path(args[args.index("-o") + 1])
        native.write_text("#!/bin/sh\n")
    return subprocess.CompletedProcess(args, 0)


@unittest.skipIf(os.name != "posix", "source installation requires POSIX")
class SkillInstallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mw-skill-install-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.source = self.tmp / "source"
        (self.source / "makewand").mkdir(parents=True)
        (self.source / "makewand" / "__init__.py").write_text("")
        (self.source / "bin").mkdir()
        (self.source / "bin" / "makewand").write_text("#!/usr/bin/env python3\n")
        (self.source / "skills" / "makewand-orchestrator").mkdir(parents=True)
        (self.source / "skills" / "makewand-orchestrator" / "SKILL.md").write_text(SKILL_TEXT)
        self.skills_root = self.tmp / "skills"
        self.installer = _load_installer()

    def _install(self):
        stderr = io.StringIO()
        with mock.patch.dict(os.environ, {"MAKEWAND_SKILLS_DIR": str(self.skills_root)}), \
                mock.patch.object(self.installer.subprocess, "check_output", _fake_check_output), \
                mock.patch.object(self.installer.subprocess, "run", _fake_run), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(stderr):
            self.installer._install_locked(self.source, self.tmp / "root", self.tmp / "bin")
        return stderr.getvalue()

    def test_skill_is_installed_once_under_its_own_name(self):
        self._install()
        self.assertEqual((self.skills_root / "makewand-orchestrator" / "SKILL.md").read_text(), SKILL_TEXT)
        self.assertEqual(sorted(p.name for p in self.skills_root.iterdir()), ["makewand-orchestrator"])
        self.assertTrue((self.tmp / "bin" / "makewand").is_file())

    def test_existing_directories_are_left_alone(self):
        user_skill = self.skills_root / "trio-orchestrator"
        user_skill.mkdir(parents=True)
        (user_skill / "SKILL.md").write_text("---\nname: trio-orchestrator\n---\nmy own skill\n")
        (user_skill / "notes.txt").write_text("keep me")
        stderr = self._install()
        self.assertEqual((user_skill / "SKILL.md").read_text(), "---\nname: trio-orchestrator\n---\nmy own skill\n")
        self.assertEqual((user_skill / "notes.txt").read_text(), "keep me")
        self.assertNotIn("duplicate", stderr)

    def test_legacy_duplicate_is_reported_not_deleted(self):
        legacy = self.skills_root / "trio-orchestrator"
        legacy.mkdir(parents=True)
        (legacy / "SKILL.md").write_text(SKILL_TEXT)
        stderr = self._install()
        self.assertTrue((legacy / "SKILL.md").is_file(), "an existing directory must not be deleted")
        self.assertIn(str(legacy), stderr)
        self.assertIn("duplicate", stderr)

    def test_legacy_copy_still_claiming_the_name_is_kept_current(self):
        for front_matter_name in ("makewand-orchestrator", '"makewand-orchestrator"', "'makewand-orchestrator'",
                                  "makewand-orchestrator  # installed by makewand"):
            with self.subTest(name=front_matter_name):
                shutil.rmtree(self.skills_root, ignore_errors=True)
                legacy = self.skills_root / "trio-orchestrator"
                legacy.mkdir(parents=True)
                (legacy / "SKILL.md").write_text(OLD_SKILL_TEXT.replace("makewand-orchestrator", front_matter_name, 1))
                (legacy / "notes.txt").write_text("keep me")
                stderr = self._install()
                self.assertEqual((legacy / "SKILL.md").read_text(), SKILL_TEXT)
                self.assertEqual((legacy / "notes.txt").read_text(), "keep me")
                self.assertIn("duplicate", stderr)

    def test_skill_quoting_the_name_in_its_body_is_left_alone(self):
        user_skill = self.skills_root / "trio-orchestrator"
        user_skill.mkdir(parents=True)
        (user_skill / "SKILL.md").write_text(USER_SKILL_TEXT)
        stderr = self._install()
        self.assertEqual((user_skill / "SKILL.md").read_text(), USER_SKILL_TEXT)
        self.assertNotIn("duplicate", stderr)

    def test_symlinked_legacy_directory_is_not_written_through(self):
        elsewhere = self.tmp / "dotfiles" / "skill"
        elsewhere.mkdir(parents=True)
        (elsewhere / "SKILL.md").write_text(OLD_SKILL_TEXT)
        self.skills_root.mkdir(parents=True)
        (self.skills_root / "trio-orchestrator").symlink_to(elsewhere, target_is_directory=True)
        stderr = self._install()
        self.assertEqual((elsewhere / "SKILL.md").read_text(), OLD_SKILL_TEXT)
        self.assertIn("duplicate", stderr)


class DeclaresSkillTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mw-skill-front-matter-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.declares = _load_installer()._declares_skill

    def check(self, text):
        path = self.tmp / "SKILL.md"
        path.write_text(text, encoding="utf-8")
        return self.declares(path, "makewand-orchestrator")

    def test_front_matter_name(self):
        for text in (SKILL_TEXT, OLD_SKILL_TEXT, '---\nname: "makewand-orchestrator"\n---\n',
                     "---\ndescription: x\nname: 'makewand-orchestrator'\n---\n",
                     "﻿---\r\nname: makewand-orchestrator\r\n---\r\n",
                     "---\nname: makewand-orchestrator # comment\n---\n"):
            with self.subTest(text=text):
                self.assertTrue(self.check(text))

    def test_names_outside_the_front_matter_or_other_names_do_not_count(self):
        for text in (USER_SKILL_TEXT, "# Skill\nname: makewand-orchestrator\n",
                     "\n---\nname: makewand-orchestrator\n---\n", "---\nname: makewand-orchestrator-extra\n---\n",
                     "---\nmetadata:\n  name: makewand-orchestrator\n---\n", "---\nname:makewand-orchestrator\n---\n",
                     "---\ndescription: x\n---\nname: makewand-orchestrator\n", ""):
            with self.subTest(text=text):
                self.assertFalse(self.check(text))
        self.assertFalse(self.declares(self.tmp / "missing.md", "makewand-orchestrator"))


if __name__ == "__main__":
    unittest.main()
