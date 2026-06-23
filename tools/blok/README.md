# Blok for Blok solver

Automation for Klub Lotto's daily "Blok for Blok" (a 1010!/Block-Blast-style
8×8 block-placement puzzle). The game is a Phaser WebGL canvas inside a
cross-origin iframe with no accessible JS game state, so it works purely by
**screenshot pixel-sampling + real coordinate mouse-drags**.

> **Now a native Go subcommand.** This was originally Python (`blok.py` +
> `blok_play.py` + `play.sh`); it was ported to Go on 2026-06-23 to match the
> other games, drop the numpy/Pillow venv, and ship as one static binary. The
> Python files are gone — the live code is:
>
> - **Perception + solver:** [`internal/klublotto/blok.go`](../../internal/klublotto/blok.go)
>   — `readBoard` → 8×8 occupancy, `readPieces` → tray shape matrices + viewport
>   pickup points, `BlokPlan` → full-trio lookahead (`120*lines_cleared +
>   quality`, quality penalises dead 1×1 holes). The colour thresholds and band
>   logic are a faithful port; a parity test
>   (`TestBlokPerceptionMatchesFixture`, fixture in
>   `internal/klublotto/testdata/blok_state.png`) pins the perception output so
>   it can't silently drift.
> - **Driver:** [`cmd/klub-lotto/blok.go`](../../cmd/klub-lotto/blok.go) —
>   open + Start spil, perceive → plan → drag → verify → read score, looped to
>   game-over.

First automated win: 2026-06-19 (score 200, daily lod earned). Best score so far:
2628 (played to game-over). See `../../wiki/games/blok-for-blok.md` for the full
write-up and learnings.

## Run
```sh
make blok            # play to game-over, maximising score
make blok GOAL=200   # stop early once the live score reaches 200 (just earn the lod)
```
Run `make login` first if the session isn't authenticated. The first time you
cross 200 the game shows the win screen and earns the lod; once earned, replays
run all the way to a real game-over while tracking a best score.

## Score reading
The current and best score live in the game's DOM inside the iframe, NOT only on
the WebGL canvas — `.game-current-score-value` / `.game-best-score-value`. The
driver reads them with a frame-scoped `eval`; being plain DOM text they're exact
and immune to the stale-canvas problem that affects board reads. After each
placed piece it records a row to `<DataDir>/blok-scores.csv`
(`step,piece,row,col,current_score,best_score,placed_cells,board_filled`).

## Gotchas
- The player and any monitoring share ONE `agent-browser` session; `frame`
  context switches race. **Never** issue agent-browser commands while a run is
  in progress — watch the log instead.
- When `readSettled` can't read the board, the driver screenshots and treats it
  as game-over: at the end of a successful run that's the **win/game-over
  screen**, not an error.
- Each browser op is wrapped in a 20s timeout so a frame switch into the game
  iframe after it's been replaced by the game-over screen can't hang the run.
