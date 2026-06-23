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
DEFAULT_VENV=/tmp/blokenv
PY="${BLOK_PYTHON:-$DEFAULT_VENV/bin/python}"
# Placed-cell budget the solver loops up to. Default huge so the run plays on
# until game-over (set BLOK_GOAL>0 to instead stop at a target score).
TARGET="${BLOK_TARGET:-100000}"
SHOTDIR="${BLOK_SHOTDIR:-$(pwd)/.klublotto}"
URL="https://danskespil.dk/klublotto/dagens-blok-for-blok"
GAME_IFRAME="iframe.kl-game__iframe"

mkdir -p "$SHOTDIR"

# Ensure a perception venv with Pillow+numpy (system PIL is blocked by PEP-668).
# Verify the deps actually IMPORT — checking only that the python binary exists
# is not enough: a stale/partial /tmp/blokenv (binary present, numpy never
# installed) would otherwise be used as-is and crash with ModuleNotFoundError.
blok_deps_ok() { "$PY" -c 'import numpy, PIL' >/dev/null 2>&1; }
if [ -n "${BLOK_PYTHON:-}" ]; then
  # Caller supplied their own interpreter — validate, don't mutate it.
  if ! blok_deps_ok; then
    echo "blok: ERROR: \$BLOK_PYTHON ($PY) cannot import numpy + PIL; install them or unset BLOK_PYTHON." >&2
    exit 1
  fi
elif [ ! -x "$PY" ] || ! blok_deps_ok; then
  echo "blok: setting up perception venv at $DEFAULT_VENV (Pillow+numpy)…"
  # --clear fully rebuilds a stale/partial venv (e.g. one created without pip);
  # use `python -m pip` (+ ensurepip) rather than the pip wrapper, which may be
  # missing in such a broken venv.
  python3 -m venv --clear "$DEFAULT_VENV"
  PY="$DEFAULT_VENV/bin/python"
  "$PY" -m ensurepip --upgrade >/dev/null 2>&1 || true
  "$PY" -m pip -q install --upgrade pip >/dev/null 2>&1 || true
  "$PY" -m pip -q install pillow numpy
  blok_deps_ok || { echo "blok: ERROR: venv setup failed to provide numpy + PIL." >&2; exit 1; }
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
