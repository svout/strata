#!/usr/bin/env bash
# PostToolUse hook: format only the file Claude just edited (fast, keeps diffs consistent).
f=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("file_path",""))' 2>/dev/null)
[ -z "$f" ] || [ ! -f "$f" ] && exit 0
case "$f" in
  *.go) gofmt -w "$f" ;;
  *.py) uv run ruff format --quiet "$f" && uv run ruff check --fix --quiet "$f" ;;
  *.ts|*.tsx|*.js|*.jsx|*.json|*.css|*.md) pnpm exec prettier --write --log-level silent "$f" ;;
esac
exit 0
