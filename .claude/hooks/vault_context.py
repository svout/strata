#!/usr/bin/env python3
"""Keep the Strata vault as Claude's memory. Claude Code hook, one script, three events:

  start   (SessionStart)      print the Now block + last Session Log entry -> added to Claude's context
  prompt  (UserPromptSubmit)  if work changed since the vault was last updated, print a /wrap-up reminder
  end     (SessionEnd)        if the session ends with unrecorded work, prepend an auto entry to the Session Log

Vault = $STRATA_VAULT, else the project dir (vault mode). Standard library only; never blocks Claude.
"""
import json, os, sys, tempfile, time
from datetime import datetime

SKIP_DIRS = {".git", "node_modules", ".venv", "venv", "__pycache__", ".next", "dist", "build", ".turbo",
             ".obsidian", ".trash", ".claude", "_archive", ".pytest_cache", ".mypy_cache", ".ruff_cache",
             "coverage", ".cache"}
RECORD_FILES = {"Session Log.md", "Home.md"}  # updating these is "recording", not "work"
LOG_MARK = "%% entries below, newest first — hooks insert here %%"


def read_input():
    try:
        return json.load(sys.stdin)
    except Exception:
        return {}


def paths(data):
    project = os.environ.get("CLAUDE_PROJECT_DIR") or data.get("cwd") or os.getcwd()
    vault = os.environ.get("STRATA_VAULT") or project
    sid = data.get("session_id") or "unknown"
    marker = os.path.join(tempfile.gettempdir(), "strata-claude", f"{sid}.start")
    return project, vault, marker


def section(text, heading, stop_prefix="## "):
    lines, out, on = text.splitlines(), [], False
    for ln in lines:
        if ln.strip() == heading:
            on = True
            continue
        if on and ln.startswith(stop_prefix):
            break
        if on:
            out.append(ln)
    return "\n".join(out).strip()


def log_body(log_text):
    if LOG_MARK in log_text:
        return log_text.split(LOG_MARK, 1)[1]
    if log_text.startswith("---\n"):  # skip frontmatter
        log_text = log_text.split("\n---\n", 1)[-1]
    return log_text.split("\n---\n", 1)[-1]


def last_log_entry(log_text):
    body = log_body(log_text)
    out, on = [], False
    for ln in body.splitlines():
        if ln.startswith("### "):
            if on:
                break
            on = True
        if on:
            out.append(ln)
    return "\n".join(out).strip()


def changed_since(root, since):
    """Work files modified after `since` (excluding record files). Returns [(mtime, relpath)] newest first."""
    hits = []
    for d, dirs, files in os.walk(root):
        dirs[:] = [x for x in dirs if x not in SKIP_DIRS]
        for f in files:
            if f in RECORD_FILES or f == ".DS_Store":
                continue
            p = os.path.join(d, f)
            try:
                m = os.path.getmtime(p)
            except OSError:
                continue
            if m > since:
                hits.append((m, os.path.relpath(p, root)))
    return sorted(hits, reverse=True)


def mtime(p):
    try:
        return os.path.getmtime(p)
    except OSError:
        return 0.0


def unrecorded(project, vault, marker):
    start = mtime(marker)
    if not start:
        return []
    log_m = mtime(os.path.join(vault, "Workflow", "Session Log.md"))
    since = max(start, log_m)
    return changed_since(project, since)


def cmd_start(data):
    project, vault, marker = paths(data)
    os.makedirs(os.path.dirname(marker), exist_ok=True)
    if not os.path.exists(marker) or data.get("source") in ("startup", "clear"):
        with open(marker, "w") as fh:
            fh.write(str(time.time()))
    home = os.path.join(vault, "Home.md")
    log = os.path.join(vault, "Workflow", "Session Log.md")
    try:
        now = section(open(home, encoding="utf-8").read(), "## Now")
    except OSError:
        print(f"[strata] Vault not found at {vault}. Set STRATA_VAULT in .claude/settings.local.json.")
        return
    try:
        last = last_log_entry(open(log, encoding="utf-8").read())
    except OSError:
        last = "(no Session Log yet)"
    print("## Strata vault context (auto-loaded at session start)\n")
    print(f"Vault: {vault}\n")
    print("### Now (from Home.md)\n" + now + "\n")
    print("### Last Session Log entry\n" + last + "\n")
    print("Rules: follow Workflow/Step Loop.md. Check Workflow/Reuse Registry.md before writing code. "
          "Finish every step with /wrap-up so the vault stays current.")


def cmd_prompt(data):
    project, vault, marker = paths(data)
    if not os.path.exists(marker):
        os.makedirs(os.path.dirname(marker), exist_ok=True)
        with open(marker, "w") as fh:
            fh.write(str(time.time()))
        return
    hits = unrecorded(project, vault, marker)
    if not hits:
        return
    names = ", ".join(h[1] for h in hits[:5]) + (f" (+{len(hits) - 5} more)" if len(hits) > 5 else "")
    print(f"[strata] {len(hits)} file(s) changed since the vault was last updated: {names}. "
          "When this step is done, run /wrap-up (spec ☑, Now, Reuse Registry, Session Log) before starting the next one.")


def cmd_end(data):
    project, vault, marker = paths(data)
    hits = unrecorded(project, vault, marker)
    try:
        os.remove(marker)
    except OSError:
        pass
    if not hits:
        return
    log = os.path.join(vault, "Workflow", "Session Log.md")
    try:
        text = open(log, encoding="utf-8").read()
    except OSError:
        return
    names = ", ".join(f"`{h[1]}`" for h in hits[:8]) + (f" (+{len(hits) - 8} more)" if len(hits) > 8 else "")
    stamp = datetime.now().strftime("%Y-%m-%d %H:%M")
    where = os.path.basename(project.rstrip("/"))
    entry = (f"### {stamp} — ⚠ unrecorded session (auto)\n"
             f"- Session in `{where}` ended without /wrap-up ({data.get('reason', 'exit')}).\n"
             f"- Changed: {names}\n"
             f"- Next: run `/next-step`, check the spec checkboxes against these files, then `/wrap-up`.\n\n")
    if LOG_MARK in text:
        head, tail = text.split(LOG_MARK, 1)
        text = head + LOG_MARK + "\n\n" + entry + tail.lstrip("\n")
    else:
        text = text.rstrip("\n") + "\n\n" + entry
    with open(log, "w", encoding="utf-8") as fh:
        fh.write(text)


if __name__ == "__main__":
    data = read_input()
    try:
        {"start": cmd_start, "prompt": cmd_prompt, "end": cmd_end}[sys.argv[1]](data)
    except Exception as exc:  # a hook must never break the session
        print(f"[strata] context hook error: {exc}", file=sys.stderr)
    sys.exit(0)
