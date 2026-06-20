# Blok for Blok solver

Automation for Klub Lotto's daily "Blok for Blok" (a 1010!/Block-Blast-style
8×8 block-placement puzzle). The game is a Phaser WebGL canvas inside a
cross-origin iframe with no accessible JS game state, so this works purely by
**screenshot pixel-sampling + real coordinate mouse-drags** via the
`agent-browser` CLI.

First automated win: 2026-06-19 (score 200, daily lod earned). Best score so far:
2628 (played to game-over). See `../../wiki/games/blok-for-blok.md` for the full
write-up and learnings.

## Files
- `blok.py` — perception. `read_board(im)` → 8×8 occupancy; `read_pieces(im, geom)`
  → tray piece shape matrices + viewport pickup points. Run standalone to debug:
  `python blok.py <screenshot.png> all`.
- `blok_play.py` — solver + executor. Full-trio lookahead greedy
  (`120*lines_cleared + quality`; quality penalises dead 1×1 holes), smooth
  stepped drags, predicted-board placement verification, receding-horizon re-read.
  `read_scores()` reads the exact live score from the DOM (see below).

## Score reading
The current and best score live in the game's DOM inside the iframe, NOT only on
the WebGL canvas — `div.game-current-score-value` and `div.game-best-score-value`.
`read_scores()` reads them with a frame-scoped `eval`; being plain DOM text they're
exact and immune to the stale-canvas problem that affects board reads. After each
placed piece the executor records a row to `<BLOK_SHOTDIR>/blok-scores.csv`
(`step,piece,row,col,current_score,best_score,placed_cells,board_filled`) and
echoes `score=… best=…` to the log. Set `BLOK_GOAL` (default 200) to stop once the
current score reaches it; set it very high to play on to game-over for a max score.

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
   The argument is a cumulative **placed-cell budget** (a fallback bound); the run
   normally stops when the live DOM score reaches `BLOK_GOAL` (default 200). The
   **first** time you cross 200 the game shows the win screen and earns the lod.
   Once the lod is earned, replays do NOT stop at 200 — the game runs on until
   game-over while tracking a best score, so set `BLOK_GOAL` very high (and a large
   placed-cell budget) to chase a maximum score.

## Gotchas
- The player and any monitoring share ONE `agent-browser` session; `frame`
  context switches race. **Never** issue agent-browser commands while the player
  runs — monitor via its `-u` log instead.
- When `read_settled` raises `could not read board`, screenshot first: at the end
  of a successful run that's the **win screen**, not an error.
