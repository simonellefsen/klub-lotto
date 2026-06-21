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
echoes `score=… best=…` to the log. By default the run plays to game-over; set
`BLOK_GOAL=<n>` (>0) to stop early once the current score reaches `<n>`.

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
   /tmp/blokenv/bin/python -u tools/blok/blok_play.py
   ```
   By default it **plays on until game-over** (the board can't take another piece),
   maximising the score. The **first** time you cross 200 the game shows the win
   screen and earns the lod; once the lod is earned, replays run all the way to a
   real game-over while tracking a best score. Set `BLOK_GOAL=<n>` to stop early
   once the live DOM score reaches `<n>` (e.g. `BLOK_GOAL=200` to just earn the
   lod); the optional positional arg caps placed cells (default huge).

## Gotchas
- The player and any monitoring share ONE `agent-browser` session; `frame`
  context switches race. **Never** issue agent-browser commands while the player
  runs — monitor via its `-u` log instead.
- When `read_settled` raises `could not read board`, screenshot first: at the end
  of a successful run that's the **win screen**, not an error.
