#!/usr/bin/env bash
# scripts/sync.sh — commit any wiki/doc changes and push to GitHub.
#
# Called by the Makefile after submitting a real quiz run, and intended to
# be safe to run by itself any time. Dry runs deliberately do not call this.
# If there's nothing to commit it exits 0 quietly.
#
# Behaviour:
#   - Only commits files under wiki/ and doc files (README.md, RUN.md,
#     AGENTS.md, *.md at repo root). Source code changes are NOT committed
#     by this script — those should go through a regular code review flow.
#   - Commit message includes the latest wiki/log.md entry when there is one.
#   - Push is optional: skipped if no `origin` is configured or no push
#     access is available (the local commit still lands).

set -euo pipefail

cd "$(dirname "$0")/.."

# Stage only docs/wiki paths.
git add -A wiki/ README.md RUN.md 2>/dev/null || true

if git diff --cached --quiet; then
  echo "sync: nothing to commit"
  exit 0
fi

# Pick the latest log entry as commit subject when available.
subject="docs: wiki/repo sync $(date -u +%Y-%m-%dT%H:%M:%SZ)"
if [[ -f wiki/log.md ]]; then
  latest=$(grep '^## \[' wiki/log.md | tail -n 1 | sed 's/^## //;s/[[:space:]]\{2,\}/ /g')
  if [[ -n "$latest" ]]; then
    subject="docs: ${latest}"
  fi
fi

git commit -m "$subject" --no-verify >/dev/null
echo "sync: committed — $subject"

if git remote get-url origin >/dev/null 2>&1; then
  if git push origin HEAD 2>/dev/null; then
    echo "sync: pushed to origin"
  else
    echo "sync: push failed (likely no auth) — commit is local"
  fi
else
  echo "sync: no origin remote configured — commit is local"
fi
