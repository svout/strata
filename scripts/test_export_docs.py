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
        self.err = io.StringIO()
        quiet.enter_context(contextlib.redirect_stderr(self.err))
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

    # --- 2b: export hardening -------------------------------------------

    def note(self, name, text):
        p = self.vault / "Specs" / name
        p.write_bytes(text.encode("utf-8") if isinstance(text, str) else text)
        return p

    def body_of(self, path):
        _, body = ed.split_frontmatter(path.read_text(encoding="utf-8"))
        return body

    def test_slug_collision_fails_and_writes_neither(self):
        self.note("SPEC-200 A.md", "# one\n")
        self.note("SPEC-200 a!.md", "# two\n")
        self.assertEqual(self.run_export(), 1)
        self.assertFalse((self.repo / "docs" / "specs" / "SPEC-200-a.md").exists())

    def test_export_refuses_target_with_other_src(self):
        self.run_export()
        # a valid export stamped from another note, placed where SPEC-099 exports to
        other = ed.render("Specs/SPEC-099 Other.md", "# other\n")
        self.out.write_bytes(other)
        self.assertEqual(self.run_export(), 1)
        self.assertEqual(self.out.read_bytes(), other)
        self.assertIn("Delete it if the old source was renamed", self.err.getvalue())

    def test_crlf_source_matches_lf_export(self):
        self.run_export()
        lf = self.out.read_bytes()
        self.src.write_bytes(NOTE.replace("\n", "\r\n").encode("utf-8"))
        self.assertEqual(self.run_export(), 0)
        self.assertEqual(self.out.read_bytes(), lf)
        self.assertNotIn(b"\r", lf)
        self.assertEqual(ed.check(self.repo), 0)

    def test_bom_source(self):
        self.run_export()
        lf = self.out.read_bytes()
        self.src.write_bytes(b"\xef\xbb\xbf" + NOTE.encode("utf-8"))
        self.assertEqual(self.run_export(), 0)
        self.assertEqual(self.out.read_bytes(), lf)

    def test_frontmatter_ending_at_eof(self):
        self.src.unlink()
        out = self.repo / "docs" / "specs" / "SPEC-300-eof.md"
        self.note("SPEC-300 EOF.md", "---\ntype: spec\n---")
        self.assertEqual(self.run_export(), 0)
        text = out.read_text(encoding="utf-8")
        self.assertTrue(text.startswith("---\ntype: spec\n---\n<!-- DO NOT EDIT"), text)
        self.assertEqual(ed.check(self.repo), 0)

    def test_four_backtick_fence_not_closed_by_three(self):
        body = ed._strip_body("````md\n```\n[[x]] %%keep%%\n```\n````\nafter [[y]] %%gone%%\n")[0]
        self.assertIn("[[x]] %%keep%%", body)
        self.assertIn("after y \n", body)
        self.assertNotIn("gone", body)

    def test_comment_spanning_fence_is_hidden(self):
        body = ed._strip_body("a %%secret1\n```\ncode secret2\n```\nstill hidden%% b\n")[0]
        self.assertNotIn("secret1", body)
        self.assertNotIn("secret2", body)
        self.assertNotIn("still hidden", body)
        self.assertEqual(body, "a  b\n")

    def test_percent_in_inline_code_is_literal(self):
        body = ed._strip_body("strip `%%…%%` here; an unclosed `%%` stays; ``a ` %% b`` too %%gone%% end\nnext line\n")[0]
        self.assertEqual(body, "strip `%%…%%` here; an unclosed `%%` stays; ``a ` %% b`` too  end\nnext line\n")
        # an unmatched backtick doesn't hide a real comment
        self.assertEqual(ed._strip_body("a ` b %%x%% c\n")[0], "a ` b  c\n")

    def test_unclosed_comment_hides_to_eof(self):
        body = ed._strip_body("keep\n%% private\nmore private\n```\nx\n```\n")[0]
        self.assertEqual(body, "keep\n")

    def test_check_fails_on_renamed_export(self):
        self.run_export()
        self.out.rename(self.repo / "docs" / "specs" / "SPEC-999-copy.md")
        self.assertEqual(ed.check(self.repo), 1)

    def test_non_utf8_source_exit1_others_exported(self):
        self.note("SPEC-400 Bad.md", b"# caf\xe9\n")
        self.assertEqual(self.run_export(), 1)
        self.assertTrue(self.out.exists())  # SPEC-099 still exported
        self.assertFalse((self.repo / "docs" / "specs" / "SPEC-400-bad.md").exists())

    # --- 2c: no silent loss ----------------------------------------------

    def test_unclosed_comment_warns_with_note_line(self):
        self.src.unlink()
        # a code span wrapped across lines: its %% opens a comment that never closes
        self.note("SPEC-500 Wrap.md", "---\ntype: spec\n---\n\nkeep\na `x\n%% y` b\nlost\n")
        self.assertEqual(self.run_export(), 0)  # Obsidian hides to EOF too: not an error
        self.assertIn("warning: Specs/SPEC-500 Wrap.md:7: %% comment opened here is never closed", self.err.getvalue())
        out = (self.repo / "docs" / "specs" / "SPEC-500-wrap.md").read_text(encoding="utf-8")
        self.assertNotIn("lost", out)

    def test_no_warning_for_closed_comments(self):
        self.assertEqual(self.run_export(), 0)
        self.assertNotIn("warning", self.err.getvalue())

    def test_u2028_does_not_split_line(self):
        body, open_line = ed._strip_body("a\u2028```\n%%secret%%\n")
        self.assertNotIn("secret", body)  # no fence opened mid-line, so the comment is stripped
        self.assertIsNone(open_line)
        fm, _ = ed.transform("---\ntitle: a\u2028exported: x\n---\nbody\n")
        self.assertIn("title: a\u2028exported: x", fm)  # U+2028 doesn't split a frontmatter value

    def test_lone_cr_is_line_break(self):
        body, _ = ed._strip_body(ed.load(b"a\r```\r%%k%%\r```\r"))
        self.assertEqual(body, "a\n```\n%%k%%\n```\n")

    def test_tilde_fence_and_backtick_info_string(self):
        body, _ = ed._strip_body("~~~ a`b\n%%keep%%\n~~~\n```a`b\n%%gone%%\n")
        self.assertIn("%%keep%%", body)  # tilde fences may have a backtick in the info string
        self.assertNotIn("gone", body)  # a backtick fence may not, so it's prose

    def test_collision_blocked_export_not_orphan(self):
        self.note("SPEC-200 A.md", "# one\n")
        self.assertEqual(self.run_export(), 0)
        existing = self.repo / "docs" / "specs" / "SPEC-200-a.md"
        before = existing.read_bytes()
        self.note("SPEC-200 a!.md", "# two\n")
        self.assertEqual(self.run_export(), 1)
        err = self.err.getvalue()
        self.assertIn("all export to", err)
        self.assertNotIn("orphan", err)
        self.assertEqual(existing.read_bytes(), before)

    def test_hand_edited_orphan(self):
        self.run_export()
        edited = self.out.read_text(encoding="utf-8").replace("Tail", "Tale")
        self.out.write_text(edited, encoding="utf-8")
        self.src.unlink()
        self.assertEqual(self.run_export(), 1)
        self.assertIn("edited by hand", self.err.getvalue())
        self.assertEqual(self.out.read_text(encoding="utf-8"), edited)
        self.assertEqual(ed.check(self.repo), 1)

    def test_slug(self):
        self.assertEqual(ed.slug("SPEC-002 Payment Core and Ledger.md"), "SPEC-002-payment-core-and-ledger.md")


if __name__ == "__main__":
    unittest.main()
