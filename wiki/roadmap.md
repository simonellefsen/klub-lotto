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
