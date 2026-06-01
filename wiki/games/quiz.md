---
kind: entity
tags: [klublotto, game, quiz]
updated: 2026-05-31T10:31:57Z
---

# Quiz

Klub Lotto's daily "tænkespil" — a single multiple-choice question.
Solving it adds tickets ('lodder') to the weekly draw.

## Stats

- rounds recorded: 6
- submitted: 1
- skipped: 5
- correct: 0
- wrong: 0
- last round: 2026-05-31T10:31:57Z

## Known patterns

_LLM/operator notes go here. Edit by hand or via an ingest session._

- Questions are typically Danish trivia (history, geography, sport, pop culture).
- 4 options is the most common layout, but 2–6 has been observed; the solver
  treats any count between 2 and 8 as valid.
- The page often auto-advances on tap — there is no separate "Indsend" click.

## Solver behaviour

`klub-lotto quiz` calls all configured LLM providers in parallel, takes the
majority vote, and clicks the corresponding option. Each run is filed under
`sources/quiz-YYYYMMDD-HHMMSS.md`.

## See also

- [Index](../index.md)
- [Log](../log.md)
- [Sources directory](../sources/)
