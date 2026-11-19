#!/usr/bin/env bash
# PostToolUse hook: format only the file Claude just edited (fast, keeps diffs consistent).
f=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null)
[ -z "$f" ] || [ ! -f "$f" ] && exit 0
case "$f" in
  *.go) gofmt -w "$f" ;;
  *.py) uv run ruff format --quiet "$f" && uv run ruff check --fix --quiet "$f" ;;
esac
# No prettier step (removed 2026-11-19, SPEC-002 18). It matched *.md and
# *.json repo-wide, generated files included, so a working prettier would
# reformat the docs/ exports (breaking their check-docs hash stamps) and
# gateway/api/openapi.json; it only did nothing because pnpm isn't installed. If
# console formatting is ever added, it is scoped to console/**/*.ts(x)
# and nothing else.
exit 0
