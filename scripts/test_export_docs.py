"""Tests for export_docs.py. Temp directories only: never the real docs/ or vault."""

import contextlib
import io
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import export_docs as ed

NOTE = """---
type: spec
phase: "[[P9 Demo Phase]]"
status: approved
exported: false
---

# SPEC-099 Demo

See [[Architecture#Data model (first cut)|the data model]], [[Threat Model#Assets]] and [[Home]].
Embed: ![[diagram.png]] %% private note %% done.
%%
multi-line
private block
%%
```go
x := "[[not a link]] %% not a comment %%"
```
Tail [[Reuse Registry]].
"""


class ExportDocsTest(unittest.TestCase):
    def setUp(self):
        # keep the script's progress and error messages out of the test output
        quiet = contextlib.ExitStack()
        quiet.enter_context(contextlib.redirect_stdout(io.StringIO()))
        quiet.enter_context(contextlib.redirect_stderr(io.StringIO()))
        self.addCleanup(quiet.close)
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.vault, self.repo = root / "vault", root / "repo"
        (self.vault / "Specs").mkdir(parents=True)
        (self.repo / "docs" / "specs").mkdir(parents=True)
        self.src = self.vault / "Specs" / "SPEC-099 Demo Feature.md"
        self.src.write_text(NOTE, encoding="utf-8")
        self.out = self.repo / "docs" / "specs" / "SPEC-099-demo-feature.md"

    def tearDown(self):
        self.tmp.cleanup()

    def run_export(self):
        return ed.export(self.vault, self.repo)

    def test_export_writes_stamped_copy(self):  # AC9
        self.assertEqual(self.run_export(), 0)
        stamp, content = ed.parse(self.out.read_text(encoding="utf-8"))
        self.assertIsNotNone(stamp)
        self.assertEqual(stamp.group("src"), "Specs/SPEC-099 Demo Feature.md")
        self.assertEqual(stamp.group("srcsha"), ed.sha256(self.src.read_bytes()))
        self.assertEqual(stamp.group("sha"), ed.sha256(content))
        self.assertEqual(ed.check(self.repo), 0)
        # the stamp sits right after the frontmatter, which stays first
        self.assertTrue(self.out.read_text(encoding="utf-8").startswith("---\n"))

    def test_reexport_is_idempotent(self):
        self.run_export()
        first = self.out.read_bytes()
        self.assertEqual(self.run_export(), 0)
        self.assertEqual(self.out.read_bytes(), first)

    def test_check_fails_on_hand_edit(self):
        self.run_export()
        self.out.write_text(self.out.read_text(encoding="utf-8").replace("Tail", "Tale"), encoding="utf-8")
        self.assertEqual(ed.check(self.repo), 1)

    def test_export_refuses_hand_edited_target(self):
        self.run_export()
        edited = self.out.read_text(encoding="utf-8").replace("Tail", "Tale")
        self.out.write_text(edited, encoding="utf-8")
        self.src.write_text(NOTE + "\nnew line\n", encoding="utf-8")
        self.assertEqual(self.run_export(), 1)
        self.assertEqual(self.out.read_text(encoding="utf-8"), edited)

    def test_check_fails_on_missing_stamp(self):
        (self.repo / "docs" / "specs" / "SPEC-100-handwritten.md").write_text("# hand made\n", encoding="utf-8")
        self.assertEqual(ed.check(self.repo), 1)

    def test_source_change_reexports(self):
        self.run_export()
        self.src.write_text(NOTE.replace("Tail", "Changed tail"), encoding="utf-8")
        self.assertEqual(self.run_export(), 0)
        text = self.out.read_text(encoding="utf-8")
        self.assertIn("Changed tail", text)
        stamp, _ = ed.parse(text)
        self.assertEqual(stamp.group("srcsha"), ed.sha256(self.src.read_bytes()))
        self.assertEqual(ed.check(self.repo), 0)

    def test_conversion_change_reexports_unchanged_source(self):
        self.run_export()
        with mock.patch.object(ed, "_link", lambda m: "LINK"):
            self.assertEqual(self.run_export(), 0)
        self.assertIn("Tail LINK.", self.out.read_text(encoding="utf-8"))
        self.assertEqual(ed.check(self.repo), 0)

    def test_orphan_export_fails(self):
        self.run_export()
        self.src.unlink()
        self.assertEqual(self.run_export(), 1)

    def test_export_requires_vault(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(ed.main(["export", "--repo", str(self.repo)]), 2)

    def test_gitkeep_is_ignored(self):
        (self.repo / "docs" / "specs" / ".gitkeep").write_text("", encoding="utf-8")
        self.assertEqual(ed.check(self.repo), 0)

    def test_transform_obsidian_syntax(self):
        fm, body = ed.transform(NOTE)
        self.assertNotIn("exported:", fm)
        self.assertIn("status: approved", fm)
        self.assertIn('phase: "P9 Demo Phase"', fm)
        self.assertIn("See the data model, Threat Model › Assets and Home.", body)
        self.assertIn("Embed: diagram.png  done.", body)
        self.assertNotIn("private", body)
        self.assertIn('x := "[[not a link]] %% not a comment %%"', body)  # fenced code untouched
        self.assertIn("Tail Reuse Registry.", body)

    def test_slug(self):
        self.assertEqual(ed.slug("SPEC-002 Payment Core and Ledger.md"), "SPEC-002-payment-core-and-ledger.md")


if __name__ == "__main__":
    unittest.main()
