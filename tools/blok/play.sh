#!/usr/bin/env bash
# tools/blok/play.sh — open today's "Blok for Blok", click "Start spil", and run
# the Python perception+solver+executor to drive the live game to the 200-point
# goal. Invoked by `make blok`. Assumes an already-logged-in agent-browser
# session (run `make login` first if needed).
#
# Env (all have sensible defaults; the Makefile passes them through):
#   AGENT_BROWSER_BIN           path to the agent-browser CLI
#   AGENT_BROWSER_SESSION       session name (default: klublotto)
#   AGENT_BROWSER_SESSION_NAME  session name (default: klublotto)
#   BLOK_PYTHON                 python with Pillow+numpy (default: /tmp/blokenv/bin/python)
#   BLOK_TARGET                 cumulative placed-cell budget (default: 190)
#   BLOK_SHOTDIR                where screenshots are written (default: ./.klublotto)
set -euo pipefail
cd "$(dirname "$0")/../.."

export _ZO_DOCTOR=0
BIN="${AGENT_BROWSER_BIN:-/Users/lindau/codex/agent-browser/cli/target/release/agent-browser}"
export AGENT_BROWSER_SESSION="${AGENT_BROWSER_SESSION:-klublotto}"
export AGENT_BROWSER_SESSION_NAME="${AGENT_BROWSER_SESSION_NAME:-klublotto}"
PY="${BLOK_PYTHON:-/tmp/blokenv/bin/python}"
# Placed-cell budget the solver loops up to. Default huge so the run plays on
# until game-over (set BLOK_GOAL>0 to instead stop at a target score).
TARGET="${BLOK_TARGET:-100000}"
SHOTDIR="${BLOK_SHOTDIR:-$(pwd)/.klublotto}"
URL="https://danskespil.dk/klublotto/dagens-blok-for-blok"
GAME_IFRAME="iframe.kl-game__iframe"

mkdir -p "$SHOTDIR"

# Bootstrap the perception venv if it's missing (system PIL is blocked by PEP-668).
if [ ! -x "$PY" ]; then
  echo "blok: creating perception venv at /tmp/blokenv (Pillow+numpy)…"
  python3 -m venv /tmp/blokenv
  /tmp/blokenv/bin/pip -q install pillow numpy
  PY=/tmp/blokenv/bin/python
fi

echo "blok: opening $URL"
"$BIN" open "$URL" >/dev/null
sleep 4

# Click the in-frame "Start spil" DIV.button if the intro screen is showing,
# then nudge the collapsed canvas back to full size. No-op if already in play.
echo "blok: starting game (Start spil)…"
"$BIN" frame "$GAME_IFRAME" >/dev/null 2>&1 || true
"$BIN" eval '(function(){var els=[...document.querySelectorAll("div,button,span")].filter(e=>/start spil/i.test(e.textContent)&&e.offsetParent);if(els.length){els[els.length-1].click();return "started";}return "no start button (already in play?)";})()' || true
sleep 1.5
"$BIN" eval '(function(){window.dispatchEvent(new Event("resize"));return 1})()' >/dev/null 2>&1 || true
"$BIN" frame main >/dev/null 2>&1 || true
sleep 1

GOAL="${BLOK_GOAL:-0}"
if [ "$GOAL" -gt 0 ] 2>/dev/null; then
  echo "blok: running solver (stop at score>=${GOAL}; shots in ${SHOTDIR})"
else
  echo "blok: running solver (playing to game-over for max score; shots in ${SHOTDIR})"
fi
BLOK_SHOTDIR="$SHOTDIR" BLOK_GOAL="$GOAL" AGENT_BROWSER_BIN="$BIN" "$PY" -u tools/blok/blok_play.py "$TARGET"

echo "blok: done — final screenshot at ${SHOTDIR}/blok_final.png"
echo "blok: verify the ✓ on the Spil & Quiz overview to confirm the lod registered."
