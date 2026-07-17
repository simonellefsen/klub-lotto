---
kind: engineering-roadmap
date: 2026-06-23
tags: [klublotto, roadmap, refactor, tech-debt]
---

# Klub Lotto — Consolidation & Improvement Roadmap

A point-in-time review (2026-06-23) of the codebase with a prioritized plan for
consolidation, refactoring, and per-game/overall improvements. The code works and
the domain logic is sound; the issues are structural and concentrated in two
files.

## The shape of the codebase today

| Signal | Value | Meaning |
|---|---|---|
| `cmd/klub-lotto/games.go` | **5,459 lines, package `main`, 0 tests** | 4 games + ~85 helpers in one untestable file |
| `internal/klublotto/wordgames.go` | **3,355 lines** | ordknude + ordkløver + shared, intermixed |
| `WaitForLoad(ctx, "networkidle")` uncapped | **24 sites** | only the 3 open paths are capped; submit/extract flows still risk the ~30s stall |
| `time.Sleep(...)` | **127** | fragile fixed delays instead of condition polling |
| `kl-game__iframe` string literals | **31** | every game re-implements frame entry; no constant |
| OpenAI-compatible providers | **4 near-identical files** | openai/xai/zai/openrouter differ only by URL + name |
| Tests in `cmd`, `llm`, `store`, `wiki`, `browser` | **0** | all logic-heavy, all untested |
| CI / `make test` | **none** | verification is manual |

The **2026-06-23 blok port is the model to copy**: pure logic (perception/solver)
in `internal/klublotto`, thin driver in `cmd`, with a parity test. Most of
`games.go` violates that — submit/prompt/parse/CSP logic lives in `package main`
where it can't be tested.

## Consolidation & refactor plan (prioritized)

### P1 — Cross-cutting reliability (low risk, high daily value) — ✅ DONE 2026-06-23
1. ✅ **Cap every `networkidle` wait.** `browser.WaitSettled(ctx)` (6s cap) now
   backs all 27 sites, not just the open paths.
2. ✅ **One iframe helper + constant.** `klublotto.GameIframe` +
   `EnterGameFrame`/`LeaveFrame`; all Go-level frame entries/exits + fallback
   lists route through them.
3. ✅ **Consolidate screen detection.** `internal/klublotto/screens.go` now holds
   `IsOrdknudeWinText` / `IsOrdKloeverWinText` / `IsDanskeSpilErrorScreen` and the
   relocated `OrdknudeSolvedViaIframe` (moved out of `cmd`). Tests in
   `screens_test.go`.
- ✅ **`make test` / `make check` + GitHub Actions CI** (build + vet + test on
  push/PR).

### P2 — Break up the giants (medium risk, do incrementally, game-by-game) — IN PROGRESS
4. ✅ **Split `games.go` per game**: `games_sudoku.go`, `games_ordknude.go`,
   `games_ordkloever.go`, `games_krydsord.go` done; `games.go` is now 530 lines of
   shared helpers (down from 5,459). Also removed dead `parseIframeCell*` helpers.
5. **Push pure logic into `internal/klublotto` and test it** as you split:
   - ✅ krydsord CSP/validation cluster → `krydsord_csp.go` (+tests): BuildKrydsordCSP,
     RenderKrydsordBoard, BuildKrydsordGrid*, CrossingCount, ParseKrydsordAnswers,
     ValidateKrydsordSolution, KrydsordMatchesPattern, KrydsordConflictSlots.
   - ✅ ordknude Wordle-scoring/notes → `ordknude_notes.go` (+tests): ScoreOrdknudeGuess,
     OrdknudeMarkSquares, MergeGuessWords, OrdknudeGuessNotes.
   - ⬜ remaining: `filterOrdknudeCandidates`, ordknude/ordkløver prompt builders,
     `colourCodeOrdKloeverLetters`, sudoku cell-selector helpers.
   `games.go` is down from 5,459 → 4,933 lines so far.
6. **Split `wordgames.go`** into `ordknude_state.go` / `ordkloever_state.go` /
   `wordcommon.go` (there's already a confusingly-separate `ordknude.go`). _(not started)_

### P3 — Provider layer (medium effort, isolated) — IN PROGRESS
7. ✅ **Collapse the OpenAI-compatible providers.** openai/xai/zai → one
   `OpenAICompatible` (`internal/llm/oaicompat.go`); NewOpenAI/NewXAI/NewZAI are
   thin constructors over it (+first `internal/llm` tests). OpenRouter kept
   separate (model-id validation).
8. ✅ **Centralize provider resolution.** The word-provider routing now lives in
   `internal/llm/resolve.go` as `Resolve(name, Keys)` — config-decoupled and
   unit-tested (all keyword/slug routes, missing-key errors, unknown name).
   `wordProvider` is a thin wrapper mapping config → `llm.Keys`. (The per-game
   *vision* providers genuinely differ, so they're left as-is — a registry there
   would be over-engineering.)

## Status (2026-06-23)

P1 ✅ · P2.4 (per-game split) ✅ · P2.5 (pure-logic extraction) ✅ for the
high-value clusters · P3 ✅. **Deferred:** P2.6 (splitting `wordgames.go`) — it's
already in the `klublotto` package, so the split is cosmetic with real
mis-classification risk; low priority. `cmd/klub-lotto` is now one
`games_common.go` + one `games_<game>.go` per game.

## Per-game roadmap

- **Sudoku** — simplest, deterministic, healthy. Just needs tests for
  cell-selector/extraction; otherwise leave it.
- **Ordknude** — win detection solid (2026-06-23). Logic testable once moved down.
  Good shape.
- **Ordkløver** — the most brittle: heavy vision dependence, the 12-attempt probe
  loop, tiered models, the error-screen recovery. Biggest robustness upside from
  **DOM-first reading** (lean on `extractKloeverKeyboardViaDOM`/`ordKloeverBoardJS`,
  vision as fallback) and skipping the wasted vision call on the launcher screen.
- **Krydsord** — most code, mostly pure (graph→CSP→LLM). High test ROI on the
  CSP/validation. Vision deconstruction is the fragile part; the learned-dictionary
  compounding (solved 2026-06-23 with no manual hints) is the real win — keep
  feeding it.
- **Blok** — clean, just ported; **live validation pending**. Future upside: beam
  search beyond the greedy single-trio lookahead for higher scores.
- **Quiz** — simple, login timing fixed (2026-06-22). One disabled test in
  `quiz_test.go` (`t.Skip`, snapshot drift) — fix and re-enable.

## Overall / infrastructure

- **`make test` + a tiny GitHub Actions CI** (build + vet + test). Nothing guards
  regressions today; given how many subtle fixes landed this week, this is the
  highest-leverage single addition.
- **Structured logging** behind a small logger instead of raw `fmt.Println`
  (artifacts are already persisted well; this just makes them greppable).
- **Don't over-abstract into a `Game` interface.** The games differ enough
  (deterministic sudoku vs. vision/LLM word games vs. pixel blok) that a forced
  common framework would hurt more than help. Share *helpers*, not a *framework*.

## Suggested sequence

Start with **P1 (1→2→3)** — low risk, improves every daily run immediately, and
`WaitSettled`/the iframe helper are prerequisites that make the P2 split cleaner.
Land **`make test` + CI** alongside P1. Then do **P2 one game at a time**, writing
tests as logic moves down. Save **P3** for the next time the provider layer is
touched.

---

# Token-usage review — ordkløver / ordknude / krydsord (2026-07-05)

A pass over every LLM prompt in the three word games, sizing where the tokens
actually go and what to change. Costs are dominated by (a) **vision output
tokens** the parser throws away, (b) **repeated static rule blocks** re-sent
every round, and (c) **uncropped screenshots**.

## Where the tokens go today

| Game | Call | Frequency | In | Out | Waste |
|---|---|---|---|---|---|
| Ordkløver | vision board read (`extractOrdKloeverViaVision`) | every `reExtract`: initial + each probe round + each phrase guess ≈ **5–10×/game** | ~1,500-tok prompt + cropped image | verbose JSON | **schema demands `board.rows[].tiles[]` (~12 tok/tile) + `keyboard.keys[]` (29 objects) + `available_letters`, but the parser only reads `category`, `attempts.used`, `pattern_rows`, `tile_count`, `correct_letters`, `incorrect_letters` → ~60–80% of output tokens discarded** |
| Ordkløver | decision (`askOrdKloeverDecision`) | each phase-2 round (≤10) ×3 retries | ~700-tok WoF rulebook + state + full candidate block, re-sent verbatim every round | small JSON | static rules re-billed every round; no caching-stable prefix (attempt counts interleaved into the rules) |
| Ordkløver | probe letters (`askOrdKloeverProbeLetters`) | miss rounds (≤4) | ~400 tok | tiny | fine |
| Ordknude | candidates (`BuildOrdknudePrompt`) | only when pool empty (pool reuse across guesses ✓) | ~600–900 tok | small | history triple-encoded (emoji row + `(absent,…)` text + derived constraints); only the derived constraints are operative |
| Ordknude | color marks | DOM-first, vision fallback only ✓ | — | — | fallback screenshot is **uncropped full page** |
| Krydsord | clue OCR (`ExtractKrydsordClues`) | 1×/puzzle, cached by crossword id ✓ | ~1,700-tok prompt (≈44 example CLUE lines) + board JPG | line format ✓ | example block is ~½ the prompt; ~10 examples would carry the same signal (mapping is deterministic Go anyway) |
| Krydsord | batch candidates | 1 call ✓ | proportional to clues | small | fine |
| Krydsord | assembler | ≤3 attempts, full basePrompt re-sent + 1 error line | 1,500–2,500 tok | answer JSON | retry re-bills the whole prompt; prefix is stable → prompt caching would cover it |
| Krydsord | targeted conflict repair | rare | tiny ✓ | tiny ✓ | the model to copy |
| Krydsord (manual graph path) | deconstruct + verify | 2 vision calls | **full-page uncropped PNG ×2** + graph | 40k max out | most expensive single calls in the repo; crop to the iframe rect |

## Plan

### Tokens-P1 — big wins, low risk — ✅ DONE 2026-07-05
1. ✅ **Ordkløver vision schema diet.** Prompt now requests only
   `{category, attempts:{used,total}, board:{pattern_rows, tile_count}, keyboard:{correct_letters, incorrect_letters}}`
   — `board.rows`, `keyboard.keys`, `available_letters`, `full_pattern` dropped
   (parser was already tolerant). Output tokens −60–80% per vision call, prompt
   roughly halved.
2. ✅ **Ordkløver DOM-first extraction.** New `extractOrdKloeverViaDOM` (board via
   `ordKloeverBoardJS` in-frame, keyboard via `ordKloeverKeyboardJS`,
   category/hint/attempts from the frame body text) is the PRIMARY path in
   `ExtractOrdKloeverState`; a parent-body win check is hoisted to the top;
   vision only runs when the DOM read is unusable (welcome/spinner/no category).
   Typical game: 5–10 vision calls → ≤1. **Live-validation note:** watch the
   `[dom-first]` log line — if category never parses from frame text, the vision
   fallback silently carries the whole game again.
3. ✅ **No re-extraction after a wrong phrase guess.** `submitAndCheck` now polls
   the parent body (free Evals, ≤3×2s) for win/error-screen; a wrong guess
   restores the pre-guess board + bumps attempts locally. Only the
   danskespil error screen still triggers a real re-extract.

### Tokens-P2 — medium — ✅ DONE 2026-07-05
4. ✅ **Cache-stable prompt structure + prompt caching.** Decision prompt
   restructured [static rules → per-game constants → per-round state];
   `internal/llm/anthropic.go` GenerateJSON attaches an ephemeral
   `cache_control` block for prompts ≥4k chars. Gemini/OpenAI/OpenRouter
   implicit caching engages on the now-stable prefix. (Vision image/text block
   ordering left unchanged — not worth the extraction-quality risk now that
   vision is the rare path.)
5. ✅ **Decision rulebook slimmed** ~700→~300 tok (5 rules → 4 terse ones, worked
   examples dropped); candidate block capped at top 8.
6. ✅ **Krydsord clue-OCR examples 44 → 11** (split-cell ×2, img=true ×2,
   1-letter REX, abbreviation, hyphenated text kept).
7. ✅ **Ordknude history de-duplicated:** one `WORD (m,m,m,m,m)` line per guess;
   emoji row and the EKSEMPEL block dropped (derived constraints carry it).

### Tokens-P3 — nice-to-have — ✅ DONE 2026-07-05
8. ✅ Shared `CropToGameIframe` helper (klublotto); used by the krydsord
   graph-path screenshot and the ordknude color-fallback (falls back to the
   full page when the rect is unavailable).
9. ✅ Full prompt echoes (`wordCandidates`, ordkløver vision, krydsord assembler)
   now gated behind `KLUBLOTTO_DEBUG`; char count + saved-file path always shown.
10. ✅ `ReasoningEffort=low` on probe-letters (fresh provider, mutated) and
    krydsord conflict-repair (shared provider, cloned before mutation).

**Expected impact:** ordkløver −70–85% (dominant), krydsord −20–30%, ordknude
−15–20% of daily LLM spend.

## Other findings (dispositioned)

- **Dead code:** `internal/klublotto/ordknude.go` is `//go:build ignore` since
  the API drift and duplicates `OrdknudeTile`/`OrdknudeGuess` definitions that
  live on in `wordgames.go` — delete it (git history keeps it).
- **Duplicates to consolidate:** `abs`/`absInt` (both in `klublotto`),
  `isImmerspieleURL` (in `cmd` AND `internal`), and the vision-raw JSON salvage
  block which exists twice (`ExtractOrdKloeverState` and again inline in
  `runOrdKloever`, games_ordkloever.go ~159–227) — keep the library copy only.
- **Stale timeout:** `wordCandidates` still allows 540s/attempt; with reasoning
  effort now bounded (2026-07-01), 180s would fail onto the retry faster.
- **P2.6 revisited:** `wordgames.go` is 3,459 lines; the prompt-builder edits in
  P1/P2 above are a natural moment to split out `prompts.go` at least.

---

# Blok solver review — real payout model & chain-aware planning (2026-07-05)

Reverse-engineered the REAL scoring from the per-step CSV (`.klublotto/
blok-scores.csv`): `bonus = Δscore − Δcells` per placement isolates the clear
payouts. **Payout: the k-th clearing placement of a chain pays `10×(k−1)`** —
only the first clear is free, then +10 each (confirmed 2026-07-07 from a
continuous 14-clear chain: 0,10,20,…,130). An earlier 07-05 read of "0,0,10,20"
was a broken-and-restarted chain (two chain-firsts), which mis-fit `10×(k−2)`;
see Blok-P1 note. One step per CLEARING PLACEMENT, escalating monotonically,
with 1–2 non-clearing placements between steps, **spanning many trios unbroken**.

## Confirmed payout model (vs what we implement)

| Rule (observed) | Simulator (`bloksim.go`) | Planner (`BlokPlan`) |
|---|---|---|
| k-th clearing placement of a chain pays **10×(k−1)** (only the FIRST clear is free) | ✅ `BlokChain.Advance` (fixed 2026-07-07) | ✅ real payout accumulated in the lookahead |
| **Multi-line simultaneous clears pay NOTHING extra** (step 15: 2 lines → 40; step 18: row+col 15 cells → 50; step 29: 2 lines → 100) | ✅ correct (per placement) | ❌ values `cl×120` per LINE → indifferent between clearing 2 lines at once vs sequencing them, when sequencing pays ~double (two chain steps) |
| **Chain persists across trios** (escalation ran unbroken through ~8 trios) | ✅ tracks `comboLen`/`sinceClear` across trios | ❌ every `BlokPlan` call starts at `clears=0, combo=0`; driver passes no chain state |
| Chain-reset window: survives gaps ≤2, DIES on the 3rd non-clearing placement (confirmed 2026-07-11, see Blok-P1b; the earlier "≤3 survives" assumption was wrong) | ✅ `Advance` uses ≤2 | ✅ same via `Advance` |

Live economics: today's 660 = 183 cell-points + ~477 chain bonuses (**~72% of
score is chain**), and a live chain pays 100+ per clear late-game. The planner
being blind to chain state is the single biggest scoring gap left.

Baseline (current weights, current sim payout): `blok-sim -n 200 -seed 1` →
mean 15,203 · median 6,623 · lod≥200 96%. (Will read lower after the payout
fix — compare like-for-like only.)

## Plan

### Blok-P1 — model the real payout, make the planner chain-aware — ✅ DONE 2026-07-05
1. ✅ **Fix the sim payout:** `BlokChain.Advance` (blok.go) is the single source
   of truth — k-th clearing placement pays `10×(k−1)`, per-placement
   (multi-line pays no extra), chain dies after 3 non-clearing placements
   (reset window corrected 2026-07-11, see Blok-P1b; was modeled as 4).
   Sim, planner, and driver all use it. Pinned by `TestBlokChainAdvance`.
   **Corrected 2026-07-07:** the 07-05 fit read `10×(k−2)` ("first TWO free")
   from a broken-and-restarted chain; the 07-07 continuous-chain live trace
   (14 clears, bonuses 0,10,20,…,130) proves `10×(k−1)` ("first ONE free").
   The 07-05 live score was still correct only because the driver's score-delta
   cross-check re-synced our counter +1 above the true clear count, cancelling
   the −1 formula error. Driver re-sync updated to `Len = obs/10 + 1`.
2. ✅ **Chain-aware planning:** `BlokPlan(board, shapes, chain BlokChain)`
   accumulates the REAL payout per clearing placement + a terminal
   `BlokWChainState×Len` future-income value. New weights `BlokWChain=1`,
   `BlokWChainState=30` (replace `BlokWCombo`). Sequencing verified by
   `TestBlokPlanSequencesClearsOverDoubleClear` (1x2-then-2x2 beats the 2x2
   double clear at chain=3).
3. ✅ **Driver chain tracking:** expected clears via `BlokApply` on the pre-read
   board; chain carried across trios; cross-checked against the observed score
   delta each step (re-syncs `Len = bonus/10 + 2` when the game disagrees);
   CSV gains `clears,chain_len,bonus_exp,bonus_obs` columns.

**A/B validation (n=150, seed 1, corrected `10×(k−1)` payout, paired seeds):**
chain-blind mean 11,460 / median 4,059 → **chain-aware mean 19,964 / median
6,638 (+74% / +64%)**, cells 422→469 (chain play survives LONGER — sequenced
single-line clears keep the board flatter), lod 94→95%. A stronger pull
(chain=3, state=60) was statistically identical — the light defaults are kept.

**Live validation 2026-07-07: score 1054 — a new record (prev 660).** The
chain ran essentially unbroken from step 5 to step 34, escalating to chain=15
(+130/clear); bonuses were 910 of the 1054. This trace is what corrected the
payout model above (only the first clear is free). With the fix, the single
step-6 cross-check re-sync that occurred would no longer fire.

### Blok-P1b — gap-rule correction + slack-aware terminal — ✅ DONE 2026-07-11

**Live validation 2026-07-11: score 15,729, chain 41 (+400) — a new record
(prev 1054), 193 moves.** The `10×(k−1)` payout held to chain 41 with no cap.
The day's ONLY 3 mispredictions (`expected bonus X, game paid 0` at steps 13,
42, 114) all shared one cause and corrected the reset window:

- **Gap rule (corrected): the chain survives at most TWO consecutive
  non-clearing placements; the THIRD kills it.** Full-game evidence: 89/89
  clears after gaps of 0–2 continued the chain; 3/3 clears after a gap of
  exactly 3 restarted at +0. `BlokChain.Advance` updated (was: survive ≤3,
  die on 4th); `TestBlokChainAdvance` re-pinned.
- **Slack-scaled terminal chain value:** `BlokPlan`'s terminal
  `BlokWChainState×Len` now scales by remaining gap slack
  `×(3−SinceClear)/3` — a chain entering the next trio at SinceClear=2 must
  clear immediately or die, so it's worth ⅓ of a fresh one. This makes the
  lookahead actively keep slack in hand near trio boundaries.

**A/B (n=500, seed 1, paired seeds, corrected gap rule):** flat terminal
mean 10,057 / median 4,640 / p90 25,639 / lod 96% → **slack-scaled mean
13,644 / median 5,258 / p90 32,383 / lod 97% (+36% mean)**, max 132,967 →
284,086. `-w-chainstate 45/60` probes were within noise (+~1% mean, −1 lod
point) — default 30 kept; a proper sweep is Blok-P2.

### Blok-P2 — survival beyond the trio + retune — ✅ DONE 2026-07-11
4. ✅ **3x3-fit safety term:** `blokAny3x3Fits` (probes with a solid 3×3 via
   the existing `blokValid`) feeds a `BlokW3x3=150` penalty into `blokQuality`
   whenever NO 3×3 region fits anywhere — the classic boxed-in-soon signal.
   Pinned by `TestBlokAny3x3Fits` (checkerboard has no 3×3 window) and
   `TestBlokQualityPenalizesNo3x3Fit`. **A/B (n=200, paired seeds): disabling
   it entirely (w-3x3=0) is significantly worse (mean 19,128→12,492, p=0.0007
   at n=200, confirmed p=0.0007 again at n=200 seed 1 on a second pass)** — the
   term is clearly load-bearing. Sweeping its magnitude (75/150/250/400/600)
   showed no significant difference from any other — the effect is "must be
   present", not "must be precisely tuned"; 150 (a superset penalty deliberately
   above `BlokWDead`'s per-cell 45, since a total lockout is far worse than one
   dead cell) is kept.
5. ✅ **`-compare` flag on blok-sim:** runs the production-default weight set
   (A) and the CLI-overridden set (B) on identical paired seeds in one
   invocation, reports per-seed delta + a two-sided sign test (normal
   approximation), and flags "not distinguishable from noise" vs a real
   verdict — replaces eyeballing two separate runs.
   **Harness also parallelized** (games fan out across `GOMAXPROCS` — they
   only read the shared `BlokW*` vars during a batch, never write them), ~2.5×
   throughput on a 12-core box (2→5 games/s).
   **Retune pass (n=200-400, paired seeds, vs the shipped defaults):**
   `BlokWTight=0` (disabled) is significantly worse (p=0.0001) — confirms the
   tight-gap penalty matters. `BlokWDead` (20/30/70/100) and `BlokWNear`
   (0/5/20/30) showed no value beating the current default with significance
   at this sample size except `BlokWDead=70` (significantly WORSE, p=0.0442) —
   this game's score distribution is extremely heavy-tailed (max scores in the
   100k-300k range vs medians in the 5k-10k range), so the sign test needs
   larger n than was practical to run interactively for anything but the
   largest effects. **Kept defaults: dead=45, tight=4, near=10** — each
   confirmed non-degenerate (can't be zeroed), no confirmed better value found.
   A longer unattended sweep (n≥1000 per point) is the natural follow-up if
   more gains are wanted here.
6. ✅ **blok-sim harness polish:** `cells:` line (mean/median + bonus share of
   score) now printed — landed as part of Blok-P1's A/B validation work above.

### Blok-P3 — distribution realism — NOT STARTED
7. ⬜ **Learn the real piece distribution:** log the full shape string (e.g.
   `011/110`) per placement in blok-scores.csv (today only HxW bounding box);
   aggregate across days; feed observed frequencies into `blokSimPieces`
   (currently uniform over unique rotations, which over-weights 4-rotation
   pieces 4× vs the 2x2/3x3 — likely why sim scores dwarf live scores).
8. ⬜ **Auto-fit payout telemetry:** a tiny analyzer over accumulated CSVs that
   re-fits (payout schedule, reset window) and flags any day the observed
   bonuses contradict the model.

**Expected impact:** chain sequencing + persistence directly targets the ~72%
of score we currently leave to luck; sim will quantify, but 1.5–3× the live
score is plausible. P1 items are pure-Go, fully testable offline before the
next live run.

---

# Krydsord robustness — assembler timeout + silent vision errors (2026-07-08)

Puzzle 21281 (crossword id) HARD-FAILED: `assembleKrydsordSolutionGrid`
attempt 1 hit `context deadline exceeded` (180s cap) — gpt-5.5 at
`OPENROUTER_REASONING_EFFORT=medium` can think past 3 minutes on a 43-slot
grid. Worse, comparing the extracted clues against the live board screenshot
revealed the grid would have been WRONG even if assembly had finished:

- **`BEGÆRET` (row 4, right edge) was dropped entirely** — absent from all 43
  extracted clues. No coverage check caught it.
- **`JÆVNE` was duplicated** (`D16` len 5 AND `D18` len 2) — only one JÆVNE is
  on the board.
- **`D19 down (7): FUGL` is a phantom** — FUGL is the (5,8) split-cell clue,
  correctly mapped to the 3-letter `D14`, then RE-USED for an unrelated 7-cell
  slot. A wrong clue on a 7-letter run poisons 7 crossings.

Root cause of the dupes/phantoms: `mapVisionCluesToSlots` assigns each slot its
nearest clue INDEPENDENTLY — no 1:1 constraint, so one vision clue can be
claimed by many slots. Root cause of the drop: vision extraction has no
coverage check against the mask's known clue-cell coordinates.

## Plan

### Krydsord-P1 — quick, targeted fixes — ✅ DONE 2026-07-08
1. ✅ **Assembler timeout + effort backoff:** each attempt now 300s; on a TIMEOUT
   the provider is cloned to `ReasoningEffort=low` for the remaining attempts
   (games_krydsord.go). The dictionary pre-fixes several slots + seeds ~60
   candidates, so low effort is usually enough.
2. ✅ **1:1 clue assignment in `mapVisionCluesToSlots`:** rewritten to a global
   greedy best-first assignment with used-sets — each vision clue serves at most
   one slot, each slot at most one clue, eligibility distance-bounded (≤2 with a
   direction match, ≤1 otherwise). Unmatched slots get an empty clue (crossings
   fill them) instead of stealing a neighbour's. Pinned by
   `TestMapVisionCluesToSlotsIsOneToOne`.
3. ✅ **Prize-icon generalization:** OCR prompt now says the small icon in the
   green top-right cell is the prize (tea bag, playing card, coin, any icon),
   never a clue.

### Krydsord-P2 — coverage-enforced extraction — ✅ DONE 2026-07-08
4. ✅ **Coverage check + targeted re-ask:** after the first pass, `ExtractKrydsordClues`
   finds length≥2 slots left without a clue (`uncoveredClueCells`) and does ONE
   focused vision re-ask (`reaskKrydsordClueCells`) for just those clue cells,
   then re-maps the union — would have recovered the dropped BEGÆRET.
5. ✅ **Validate-before-cache:** the caller only writes the clue cache when
   `KrydsordClueCoverage` reports full coverage, so a partial read isn't pinned
   for the rest of the day (a re-run re-reads the missed cells).

### Krydsord — untrusted-dict-drop leak (2026-07-09) — ✅ FIXED
Puzzle 21282 was submitted with the non-word **MATEMDTIK** and rejected. The
learned dict had `VITAMIN→D` (from an earlier puzzle; today's answer was A —
both are valid vitamin answers, the dict was just incomplete). The conflict
valve fired correctly ("dropping dict answer D21=D") but only removed it from
`knownAnswers`; the stale letter survived in THREE other places, and each
pushed the model to preserve the D by mangling the crossing word instead:
1. the repair pattern (`KrydsordConflictSlots` baked the disputed slot's own
   letter into the other side's mønster),
2. the retry basePrompt (built once, before the drop — stale `fixed` cells),
3. the slot's candidate list (`cands=[D]`) + the model's own clue knowledge
   re-answering VITAMIN→D.

Fix: `KrydsordConflictSlots` patterns now exclude letters from OTHER INVOLVED
slots (trusted crossings only; pinned by
`TestKrydsordConflictSlotsExcludesDisputedLetters`); the assemble prompt +
fixed cells are REBUILT every attempt from current `knownAnswers`; a dropped
answer is purged from the slot's candidates; and a dropped 1-LETTER slot cedes
its cell to the crossing word entirely (clue blanked, answer scrubbed from all
later parses/repairs) with an immediate re-validate — today's case resolves
right after the drop with MATEMATIK intact, no repair call needed. The dict
self-heals on the next successful solve (auto-learn adds VITAMIN→A, making the
clue ambiguous → seeded as candidates, never fixed).

### Krydsord — dict-drop re-proposal on MULTI-letter slots (2026-07-14) — ✅ FIXED
Puzzle rejected with the non-word **YON** (EVIGHED should be ÆON). Same family
as the 07-09 leak, but through the multi-letter gap the 07-09 fix left open:
dict `MÅNEFASE→NY` was correctly dropped ("it conflicts with the crossings"),
but (a) the already-FORCED `answers[D14]=NY` was only deleted for 1-letter
slots, so the immediate post-drop re-validate still saw the conflict, and
(b) nothing stopped the model re-proposing NY from its own prior on the next
attempt — so it "fixed" the CROSSING instead (ÆON→YON) and produced a
"consistent" wrong grid. The crossings already spelled the correct answer:
N (TOLERANTE) + Æ (ÆON) = **NÆ** — månefase has two valid answers, ny og næ.

Fix (generalises the 1-letter cession):
1. a dropped dict answer is now removed from `answers` for EVERY slot length,
   so the post-drop re-validate lets the crossings decide — today's case
   resolves immediately after the drop, no repair call, ÆON intact;
2. `droppedAnswers` tracks the rejected word(s) per slot: the assemble prompt
   marks the slot `IKKE=[NY]` with an explicit "don't mangle the crossings to
   save a rejected answer" instruction, and any re-proposal of the dropped
   word (main parse or repair output) is scrubbed so crossings decide;
3. data: `MÅNEFASE` now lists both `NY` and `NÆ` in the learned dict → two
   matches → never pinned as a constraint again, both seeded as candidates.

### Krydsord — Norwegian answer poisoned a whole neighborhood (2026-07-15) — ✅ FIXED
Grid rejected: D10 (PERIODE, 3) came back as **UKE — Norwegian; Danish is
UGE** — and it didn't stay contained. The model coherently built a
UKE-compatible cluster around it: A11 GI→KI, A12 TO→NO, D17 IDOLET→IDOLEN
all flipped from correct to wrong to keep the grid "consistent", and the
conflict valve even dropped the CORRECT dict answer A11=GI because the
contaminated crossings outvoted it. One Norwegian word, four casualties.

Two gaps lined up:
1. The dict pinned PERIODE→NAT (multi-answer entry, but only NAT is length
   3 — the pin rule filters by slot length first). The valve correctly
   dropped it, but D10's middle cell (R7C6) is crossed by NOTHING, so that
   letter was completely unconstrained — the consistency check had no
   opinion on K vs G. Fixed on data: PERIODE now also lists UGE (two
   length-3 matches → never pinned, both seeded).
2. The assemble/repair prompts only said "ét dansk svar" in passing — the
   per-clue candidates prompt has an explicit "ALDRIG svenske/norske/
   engelske former" rule, but the assembler (which invented UKE on the
   retry) did not. Both prompts now carry the explicit anti-Scandinavian
   rule with today's exact example (UGE, ikke det norske UKE).

**Verified live same day: re-run solved on attempt 1 with ZERO drops** —
A11=GI, A12=TO, D17=IDOLET, D10=UGE, grid accepted, 23 new dict entries
learned. Forcing Danish resolved the entire cluster at once.

Optional hardening noted, not yet needed: a missing-slots mini-repair (ask
ONLY for slots left blank after a dict drop, like the conflict repair does
for conflicts) would keep a full-grid retry — and its reshuffling freedom —
out of the loop entirely when the only defect is an unfilled cell.

⬜ **Krydsord-P4 — DDO soft-check of model answers:** after a "consistent"
grid is found, validate MODEL-invented answers (not dict-seeded ones, not
1-2 letter abbreviations/roman numerals) against ordnet.dk/ddo (the
ordknude IsDDOWord helper, fail-open). On misses, one extra assemble retry
with the suspicious words flagged (IKKE= style); accept if the retry
doesn't improve. Would have caught UKE deterministically — the prompt fix
is a nudge, not a guarantee. Needs care: inflected forms (OSEDE, IDOLET)
resolve fine in DDO, but crossword-isms (EKSE) and image answers may not.

### Krydsord — token audit of a full solve (2026-07-17) — vision fixed, assemble measured
Mapped the day's 4 OpenRouter calls against actual completion tokens (now
logged per call): vision OCR 5,945 out (~700 real clue lines, ~5,200 hidden
reasoning — 88% of the bill was thinking on a transcription task), re-ask
543, candidates batch 2,854 (mostly legit), assemble 5,438 (~350-token JSON
+ ~5,100 reasoning). The board image sent to vision IS the vendor API's own
base64 JPEG (1002×1102, 95 kB) — no cleaner/cheaper source exists.

Shipped: `OpenRouterVision` gained ReasoningEffort + MaxTokens (it had
neither — vision calls were also exposed to the credits-affordability
gate); krydsord clue OCR now defaults to effort=low, cap 16000. A/B on the
day's board with 43 known-correct clues: **1,794+137 completion tokens vs
5,945+543 (−70%), 44/44 coverage — one better than the default-effort run
(which missed D14 even after the re-ask); only nit an Ø/O slip (LØVLIGE).**

Measured but NOT shipped: assemble at effort=low (−48%, consistent 75/75
first shot) — rejected because it silently misspelled D1 YNGELPLEJE →
YNGELPLEGE to make its A15 choice fit; an internally-consistent non-word
is the exact failure mode the validator can't see. Assemble stays at the
configured default (medium); its real savings are a cheaper model tier
and, structurally, Krydsord-P3 below (zero assemble tokens).

### Krydsord-P3 — deterministic CSP assembly — planned
6. ⬜ **Go backtracking assembler over the candidate lists:** we already have
   slots/lengths/crossings + dictionary seeds + batch candidates. A small
   propagate-and-recurse solver assembles most days instantly, deterministically,
   timeout-proof; only slots with no consistent candidate go to the LLM in a
   tiny repair prompt. As the learned dictionary compounds, the LLM's share of
   assembly trends toward zero.
