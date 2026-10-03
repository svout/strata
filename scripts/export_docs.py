#!/usr/bin/env python3
"""One-way export of vault specs into the repo (ADR-107).

    export  copy every vault `Specs/SPEC-*.md` into `docs/specs/`, converted to
            plain Markdown and stamped with SHA-256 hashes. Needs STRATA_VAULT.
    check   verify every export under `docs/specs/` is stamped, unedited and
            named after its source. Needs no vault, so CI runs it (via
            `make lint`).

The vault is the source of truth: exports are never edited by hand (repo
CLAUDE.md) and nothing is ever written back to the vault. The stamp is one
HTML comment straight after the frontmatter:

    <!-- DO NOT EDIT: exported from vault "<path>" by make export-docs
         | source-sha256: <hash of the vault note, normalised: UTF-8, no BOM, LF>
         | content-sha256: <hash of this file without the stamp line> -->

(on one line). It carries no date, so re-exporting an unchanged note gives
identical bytes. Exit codes: 0 ok, 1 drift / hand-edit / orphan / missing
stamp / collision / bad encoding, 2 usage error (e.g. STRATA_VAULT unset).

Standard library only; keep it Python 3.9-compatible (macOS system python3).
"""

import argparse
import hashlib
import os
import re
import sys
from pathlib import Path, PurePosixPath
from typing import Dict, List, Optional, Tuple

REPO = Path(__file__).resolve().parent.parent

# (vault directory, glob, repo directory). ADRs can be added here later.
EXPORTS = [("Specs", "SPEC-*.md", "docs/specs")]

STAMP_RE = re.compile(
    r'^<!-- DO NOT EDIT: exported from vault "(?P<src>[^"]+)" by make export-docs'
    r" \| source-sha256: (?P<srcsha>[0-9a-f]{64})"
    r" \| content-sha256: (?P<sha>[0-9a-f]{64}) -->$"
)
# Frontmatter: '---' line, optional body, closing '---' line or '---' at EOF.
FRONTMATTER_RE = re.compile(r"\A---\n(?:.*?\n)?---(?:\n|\Z)", re.DOTALL)
# A fence opens with 3+ backticks or tildes (any indent, so fences inside list
# items count); a backtick fence's info string may not contain a backtick.
FENCE_OPEN_RE = re.compile(r"^[ \t]*(`{3,}|~{3,})(.*)$")
WIKILINK_RE = re.compile(r"!?\[\[([^\]|#]*)(?:#([^\]|]*))?(?:\|([^\]]*))?\]\]")


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def load(raw: bytes) -> str:
    """Decode a note: strict UTF-8, drop one BOM, CRLF and lone CR to LF.

    Raises UnicodeDecodeError for non-UTF-8 input.
    """
    text = raw.decode("utf-8")
    if text.startswith("﻿"):
        text = text[1:]
    return text.replace("\r\n", "\n").replace("\r", "\n")


def slug(filename: str) -> str:
    """'SPEC-002 Payment Core and Ledger.md' -> 'SPEC-002-payment-core-and-ledger.md'."""
    stem = filename[:-3] if filename.endswith(".md") else filename
    m = re.match(r"(SPEC-\d+)\s*(.*)", stem)
    ident, title = (m.group(1), m.group(2)) if m else ("", stem)
    words = re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-")
    return "-".join(p for p in (ident, words) if p) + ".md"


def _link(m: "re.Match[str]") -> str:
    note, heading, alias = m.group(1).strip(), m.group(2), m.group(3)
    if alias:
        return alias.strip()
    if heading:
        return f"{note} › {heading.strip()}" if note else heading.strip()
    return note


def split_frontmatter(text: str) -> Tuple[str, str]:
    """Return (frontmatter including both '---' lines and a final newline, body)."""
    m = FRONTMATTER_RE.match(text)
    if not m:
        return "", text
    fm = m.group(0)
    return (fm if fm.endswith("\n") else fm + "\n"), text[m.end():]


def _fence_open(line: str) -> Optional[Tuple[str, int]]:
    m = FENCE_OPEN_RE.match(line.rstrip("\n"))
    if not m or (m.group(1)[0] == "`" and "`" in m.group(2)):
        return None
    return m.group(1)[0], len(m.group(1))


def _fence_closes(line: str, char: str, length: int) -> bool:
    s = line.rstrip("\n").strip(" \t")
    return len(s) >= length and set(s) == {char}


def _comment_start(text: str) -> int:
    """Index of the first %% outside an inline code span, or -1.

    A span opens with a run of N backticks and closes at the next run of
    exactly N (CommonMark); `%%` inside one is literal, as in Obsidian. An
    unmatched backtick run is literal text.
    """
    i = 0
    while i < len(text):
        if text[i] == "`":
            n = len(text[i:]) - len(text[i:].lstrip("`"))
            j, close = i + n, -1
            while True:
                j = text.find("`", j)
                if j == -1:
                    break
                m = len(text[j:]) - len(text[j:].lstrip("`"))
                if m == n:
                    close = j + m
                    break
                j += m
            i = close if close != -1 else i + n
        elif text.startswith("%%", i):
            return i
        else:
            i += 1
    return -1


def _strip_body(body: str) -> str:
    """Remove %% comments %% and flatten [[wikilinks]] outside fenced code.

    One left-to-right scan. Outside a comment, a fence is copied verbatim to
    its matching close (or EOF), and %% inside an inline code span is literal.
    Inside a comment everything is hidden up to the next %%, fences included
    ("the comment wins"); an unclosed %% hides the rest of the note, as
    Obsidian does.
    """
    out: List[str] = []
    prose: List[str] = []
    fence: Optional[Tuple[str, int]] = None
    in_comment = False

    def flush() -> None:
        out.append(WIKILINK_RE.sub(_link, "".join(prose)))
        prose.clear()

    for line in body.splitlines(keepends=True):
        if fence is not None:
            out.append(line)
            if _fence_closes(line, *fence):
                fence = None
            continue
        rest, at_line_start = line, True
        while rest:
            if in_comment:
                end = rest.find("%%")
                if end == -1:
                    rest = ""
                    break
                in_comment, rest, at_line_start = False, rest[end + 2:], False
                continue
            if at_line_start:
                opened = _fence_open(rest)
                if opened:
                    flush()
                    out.append(rest)
                    fence, rest = opened, ""
                    break
            start = _comment_start(rest)
            if start == -1:
                prose.append(rest)
                rest = ""
            else:
                prose.append(rest[:start])
                in_comment, rest, at_line_start = True, rest[start + 2:], False
    flush()
    return "".join(out)


def transform(text: str) -> Tuple[str, str]:
    """Convert a (normalised) vault note to plain Markdown: (frontmatter, body).

    Strips %% comments %% and turns [[wikilinks]] into text (body and
    frontmatter values), drops the vault-only `exported:` key, and leaves
    fenced code untouched.
    """
    fm, body = split_frontmatter(text)
    fm = "".join(WIKILINK_RE.sub(_link, line) for line in fm.splitlines(keepends=True) if not line.startswith("exported:"))
    return fm, _strip_body(body)


def render(rel_src: str, text: str) -> bytes:
    fm, body = transform(text)
    content = (fm + body).encode("utf-8")
    stamp = (
        f'<!-- DO NOT EDIT: exported from vault "{rel_src}" by make export-docs'
        f" | source-sha256: {sha256(text.encode('utf-8'))} | content-sha256: {sha256(content)} -->\n"
    )
    return (fm + stamp + body).encode("utf-8")


def parse(text: str) -> Tuple[Optional["re.Match[str]"], bytes]:
    """Return (stamp match or None, file bytes without the stamp line)."""
    fm, body = split_frontmatter(text)
    first, _, rest = body.partition("\n")
    m = STAMP_RE.match(first)
    if not m:
        return None, text.encode("utf-8")
    return m, (fm + rest).encode("utf-8")


def _read_export(path: Path) -> Tuple[Optional["re.Match[str]"], Optional[str]]:
    """Read an export as-is (no normalisation: exports are LF-only UTF-8).

    Returns (stamp, problem).
    """
    try:
        text = path.read_bytes().decode("utf-8")
    except UnicodeDecodeError as e:
        return None, f"not valid UTF-8 (byte {e.start})"
    stamp, content = parse(text)
    if stamp is None:
        return None, "no export stamp (not produced by make export-docs)"
    if sha256(content) != stamp.group("sha"):
        return stamp, "edited by hand since export (content hash mismatch)"
    return stamp, None


def _stamp_problem(path: Path, stamp: "re.Match[str]", src_dir: str) -> Optional[str]:
    src = PurePosixPath(stamp.group("src"))
    if src.parent.as_posix() != src_dir:
        return f'stamp source "{src}" is not under the vault folder {src_dir}/'
    if path.name != slug(src.name):
        return f'file name does not match its source "{src}" (expected {slug(src.name)})'
    return None


def check(repo: Path) -> int:
    errors = 0
    for src_dir, _, dest in EXPORTS:
        for path in sorted((repo / dest).glob("*.md")):
            stamp, problem = _read_export(path)
            if not problem and stamp is not None:
                problem = _stamp_problem(path, stamp, src_dir)
            if problem:
                print(f"check-docs: {path.relative_to(repo)}: {problem}", file=sys.stderr)
                errors += 1
    return 1 if errors else 0


def export(vault: Path, repo: Path) -> int:
    errors = 0
    for src_dir, pattern, dest in EXPORTS:
        dest_dir = repo / dest
        dest_dir.mkdir(parents=True, exist_ok=True)
        sources = {f"{src_dir}/{p.name}": p for p in sorted((vault / src_dir).glob(pattern))}

        # Two notes that slug to the same file (case-insensitively: macOS file
        # systems ignore case) would overwrite each other: export neither.
        by_slug: Dict[str, List[str]] = {}
        for rel, src in sources.items():
            by_slug.setdefault(slug(src.name).lower(), []).append(rel)
        blocked = set()
        reported = set()  # targets already reported below, so the orphan pass skips them
        for name, rels in by_slug.items():
            if len(rels) > 1:
                print(f"export-docs: {', '.join(rels)} all export to {dest}/{name}; rename one", file=sys.stderr)
                blocked.update(rels)
                errors += 1

        for rel, src in sources.items():
            if rel in blocked:
                continue
            try:
                text = load(src.read_bytes())
            except UnicodeDecodeError as e:
                print(f"export-docs: {rel}: not valid UTF-8 (byte {e.start})", file=sys.stderr)
                errors += 1
                continue
            target = dest_dir / slug(src.name)
            new = render(rel, text)
            if target.exists():
                stamp, problem = _read_export(target)
                if not problem and stamp is not None and stamp.group("src") != rel:
                    problem = f'it was exported from "{stamp.group("src")}", not "{rel}"'
                if problem:
                    print(f"export-docs: refusing to overwrite {target.relative_to(repo)}: {problem}."
                          " Move your change into the vault note, delete the file, and re-run.", file=sys.stderr)
                    errors += 1
                    reported.add(target)
                    continue
                # Skip only if nothing would change. Comparing rendered bytes
                # (not just the source hash) also refreshes exports after a
                # change to this script's conversion rules.
                if target.read_bytes() == new:
                    continue
            target.write_bytes(new)
            print(f"export-docs: {rel} -> {target.relative_to(repo)}")

        for path in sorted(dest_dir.glob("*.md")):
            if path in reported:
                continue
            stamp, problem = _read_export(path)
            if stamp is not None and not problem:
                problem = _stamp_problem(path, stamp, src_dir)
                if not problem and stamp.group("src") not in sources:
                    problem = "its vault source no longer exists (orphan)"
            elif stamp is None and problem and problem.startswith("no export stamp"):
                problem = "unstamped file (no vault source)"
            if problem:
                print(f"export-docs: {path.relative_to(repo)}: {problem}", file=sys.stderr)
                errors += 1
    return 1 if errors else 0


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("command", choices=["export", "check"])
    p.add_argument("--repo", type=Path, default=REPO)
    p.add_argument("--vault", type=Path, default=None, help="defaults to $STRATA_VAULT")
    args = p.parse_args(argv)

    if args.command == "check":
        return check(args.repo)

    vault = args.vault or (Path(os.environ["STRATA_VAULT"]) if os.environ.get("STRATA_VAULT") else None)
    if vault is None or not vault.is_dir():
        print("export-docs: set STRATA_VAULT to the vault directory"
              " (e.g. make export-docs STRATA_VAULT=\"$HOME/Documents/Obsidian Vault/Strata\")", file=sys.stderr)
        return 2
    return export(vault, args.repo)


if __name__ == "__main__":
    sys.exit(main())
