# Blok for Blok solver

Automation for Klub Lotto's daily "Blok for Blok" (a 1010!/Block-Blast-style
8×8 block-placement puzzle). The game is a Phaser WebGL canvas inside a
cross-origin iframe with no accessible JS game state, so this works purely by
**screenshot pixel-sampling + real coordinate mouse-drags** via the
`agent-browser` CLI.

First automated win: 2026-06-19 (score 200, daily lod earned). See
`../../wiki/games/blok-for-blok.md` for the full write-up and learnings.

## Files
- `blok.py` — perception. `read_board(im)` → 8×8 occupancy; `read_pieces(im, geom)`
  → tray piece shape matrices + viewport pickup points. Run standalone to debug:
  `python blok.py <screenshot.png> all`.
- `blok_play.py` — solver + executor. Full-trio lookahead greedy
  (`120*lines_cleared + quality`; quality penalises dead 1×1 holes), smooth
  stepped drags, predicted-board placement verification, receding-horizon re-read.

## Setup
```sh
python3 -m venv /tmp/blokenv && /tmp/blokenv/bin/pip install pillow numpy
```
(System PIL is blocked by PEP-668, hence the venv. Recreate if `/tmp` was wiped.)

## Run
1. Open the game and click "Start spil" (synthetic `.click()` on the in-frame
   "Start spil" DIV works), so the empty board + tray is showing.
2. ```sh
   BLOK_SHOTDIR=/path/for/screenshots \
   /tmp/blokenv/bin/python -u tools/blok/blok_play.py 190
   ```
   The argument is a cumulative **placed-cell budget** — a safe lower bound on
   score (score = placed cells + combo bonuses). ~190 reaches the 200-point goal,
   at which the game **auto-completes** (board replaced by the win screen).

## Gotchas
- The player and any monitoring share ONE `agent-browser` session; `frame`
  context switches race. **Never** issue agent-browser commands while the player
  runs — monitor via its `-u` log instead.
- When `read_settled` raises `could not read board`, screenshot first: at the end
  of a successful run that's the **win screen**, not an error.
