#!/usr/bin/env python3
"""One-way export of vault specs into the repo (ADR-107).

    export  copy every vault `Specs/SPEC-*.md` into `docs/specs/`, converted to
            plain Markdown and stamped with SHA-256 hashes. Needs STRATA_VAULT.
    check   verify every export under `docs/specs/` is stamped and unedited.
            Needs no vault, so CI runs it (via `make lint`).

The vault is the source of truth: exports are never edited by hand (repo
CLAUDE.md) and nothing is ever written back to the vault. The stamp is one
HTML comment straight after the frontmatter:

    <!-- DO NOT EDIT: exported from vault "<path>" by make export-docs
         | source-sha256: <hash of the vault note>
         | content-sha256: <hash of this file without the stamp line> -->

(on one line). It carries no date, so re-exporting an unchanged note gives
identical bytes. Exit codes: 0 ok, 1 drift / hand-edit / orphan / missing
stamp, 2 usage error (e.g. STRATA_VAULT unset).

Standard library only; keep it Python 3.9-compatible (macOS system python3).
"""

import argparse
import hashlib
import os
import re
import sys
from pathlib import Path
from typing import List, Optional, Tuple

REPO = Path(__file__).resolve().parent.parent

# (vault directory, glob, repo directory). ADRs can be added here later.
EXPORTS = [("Specs", "SPEC-*.md", "docs/specs")]

STAMP_RE = re.compile(
    r'^<!-- DO NOT EDIT: exported from vault "(?P<src>[^"]+)" by make export-docs'
    r" \| source-sha256: (?P<srcsha>[0-9a-f]{64})"
    r" \| content-sha256: (?P<sha>[0-9a-f]{64}) -->$"
)
FENCE_RE = re.compile(r"^\s*(```|~~~)")
COMMENT_RE = re.compile(r"%%.*?%%", re.DOTALL)
WIKILINK_RE = re.compile(r"!?\[\[([^\]|#]*)(?:#([^\]|]*))?(?:\|([^\]]*))?\]\]")


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


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


def _plain(text: str) -> str:
    return WIKILINK_RE.sub(_link, COMMENT_RE.sub("", text))


def split_frontmatter(text: str) -> Tuple[str, str]:
    """Return (frontmatter including both '---' lines, body)."""
    if text.startswith("---\n"):
        end = text.find("\n---\n", 4)
        if end != -1:
            return text[: end + 5], text[end + 5 :]
    return "", text


def transform(text: str) -> Tuple[str, str]:
    """Convert a vault note to plain Markdown. Returns (frontmatter, body).

    Strips %% comments %%, turns [[wikilinks]] into text (body and frontmatter
    values), drops the vault-only `exported:` key, and leaves fenced code
    untouched.
    """
    fm, body = split_frontmatter(text)
    fm = "".join(WIKILINK_RE.sub(_link, line) for line in fm.splitlines(keepends=True) if not line.startswith("exported:"))

    out: List[str] = []
    prose: List[str] = []
    fence: Optional[str] = None
    for line in body.splitlines(keepends=True):
        m = FENCE_RE.match(line)
        if fence is None and m:
            out.append(_plain("".join(prose)))
            prose = []
            fence = m.group(1)
            out.append(line)
        elif fence is not None:
            out.append(line)
            if m and m.group(1) == fence:
                fence = None
        else:
            prose.append(line)
    out.append(_plain("".join(prose)))
    return fm, "".join(out)


def render(rel_src: str, raw: bytes) -> bytes:
    fm, body = transform(raw.decode("utf-8"))
    content = (fm + body).encode("utf-8")
    stamp = (
        f'<!-- DO NOT EDIT: exported from vault "{rel_src}" by make export-docs'
        f" | source-sha256: {sha256(raw)} | content-sha256: {sha256(content)} -->\n"
    )
    return (fm + stamp + body).encode("utf-8")


def parse(text: str) -> Tuple[Optional["re.Match[str]"], bytes]:
    """Return (stamp match or None, file bytes without the stamp line)."""
    fm, body = split_frontmatter(text)
    first, sep, rest = body.partition("\n")
    m = STAMP_RE.match(first)
    if not m:
        return None, text.encode("utf-8")
    return m, (fm + rest).encode("utf-8")


def _verify(path: Path) -> Tuple[Optional["re.Match[str]"], Optional[str]]:
    stamp, content = parse(path.read_text(encoding="utf-8"))
    if stamp is None:
        return None, "no export stamp (not produced by make export-docs)"
    if sha256(content) != stamp.group("sha"):
        return stamp, "edited by hand since export (content hash mismatch)"
    return stamp, None


def check(repo: Path) -> int:
    errors = 0
    for _, _, dest in EXPORTS:
        for path in sorted((repo / dest).glob("*.md")):
            _, problem = _verify(path)
            if problem:
                print(f"check-docs: {path.relative_to(repo)}: {problem}", file=sys.stderr)
                errors += 1
    return 1 if errors else 0


def export(vault: Path, repo: Path) -> int:
    errors = 0
    for src_dir, pattern, dest in EXPORTS:
        dest_dir = repo / dest
        dest_dir.mkdir(parents=True, exist_ok=True)
        sources = {}
        for src in sorted((vault / src_dir).glob(pattern)):
            sources[f"{src_dir}/{src.name}"] = src

        for rel, src in sources.items():
            target = dest_dir / slug(src.name)
            new = render(rel, src.read_bytes())
            if target.exists():
                _, problem = _verify(target)
                if problem:
                    print(f"export-docs: refusing to overwrite {target.relative_to(repo)}: {problem}."
                          " Move your change into the vault note, delete the file, and re-run.", file=sys.stderr)
                    errors += 1
                    continue
                # Skip only if nothing would change. Comparing rendered bytes
                # (not just the source hash) also refreshes exports after a
                # change to this script's conversion rules.
                if target.read_bytes() == new:
                    continue
            target.write_bytes(new)
            print(f"export-docs: {rel} -> {target.relative_to(repo)}")

        for path in sorted(dest_dir.glob("*.md")):
            stamp, _ = parse(path.read_text(encoding="utf-8"))
            if stamp is None or stamp.group("src") not in sources:
                print(f"export-docs: {path.relative_to(repo)} has no vault source (orphan or unstamped)", file=sys.stderr)
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
