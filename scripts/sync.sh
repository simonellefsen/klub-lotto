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

# The only paths this script may commit. Everything else — source especially —
# is off-limits, per the contract in the header comment.
DOC_PATHS=(wiki/ README.md RUN.md)

# Stage only docs/wiki paths.
git add -A -- "${DOC_PATHS[@]}" 2>/dev/null || true

# Scope the "anything to do?" check to those same paths. An unscoped check sees
# files somebody else has staged and falls through to the commit below, which is
# half of how unrelated work used to get swept up.
if git diff --cached --quiet -- "${DOC_PATHS[@]}"; then
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

# Commit ONLY the doc paths. Without this pathspec `git commit` writes the whole
# index, so any source file a human or agent happened to have staged when a game
# run fired would ride along into a "docs:" commit and get pushed — which is
# exactly what happened on 2026-09-01, when cd5e506 ("docs: … ingest | sudoku")
# swallowed six unrelated Go files mid-review. The pathspec form leaves anything
# staged outside DOC_PATHS staged and uncommitted, as the header promises.
git commit -m "$subject" --no-verify -- "${DOC_PATHS[@]}" >/dev/null
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
