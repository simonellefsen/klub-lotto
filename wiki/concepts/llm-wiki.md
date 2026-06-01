---
kind: concept
tags: [meta, knowledge-base, llm-wiki, qmd]
updated: 2026-05-31T00:00:00Z
---

# The LLM-Wiki pattern (Karpathy)

This project follows the [LLM Wiki](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
pattern: instead of plain RAG over raw documents, we maintain a
**persistent, compounding wiki** that an LLM keeps up to date as new
sources arrive. Every `klub-lotto quiz` run is one such "source".

## Three layers (verbatim from the gist)

1. **Raw sources** — immutable. For us: `wiki/sources/quiz-*.md`.
2. **The wiki** — LLM-owned synthesis. For us: `wiki/games/`, `wiki/concepts/`.
3. **The schema** — tells the LLM how layer 1 maps to layer 2. For us:
   [`wiki/AGENTS.md`](../AGENTS.md).

## Operations

- **Ingest**: automatic on every quiz run, or manual via
  `klub-lotto wiki ingest --file path/to/article.md`.
- **Query**: `klub-lotto wiki query "..."` shells out to `qmd` if
  installed, otherwise a naive grep.
- **Lint**: `klub-lotto wiki lint` (skeleton; add checks here as the
  wiki grows).

## Why this project benefits

Klub Lotto repeats: there's a Quiz every day, the same UI patterns, the
same kinds of trivia. Plain LLM calls re-derive the strategy each time.
A wiki accumulates it:

- Per-provider accuracy by topic ("Gemini wins on geography").
- UI gotchas observed over weeks (consent banner variants, auto-advance
  behaviour).
- Streak/payout rules as we figure them out.
- The "right" prompt template, refined source by source.

## qmd integration

[qmd-rust](https://github.com/simonellefsen/qmd-rust) is a local
search engine over markdown collections (BM25 + future hybrid vector
search). Hook the wiki up once:

```bash
qmd collection add wiki/ --name klublotto
qmd context add qmd://klublotto "klub-lotto wiki: games, sources, concepts"
qmd search "auto advance"
qmd query "what does the quiz UI do after submission"   # better once vector search lands
```

`klub-lotto wiki query` will route through `qmd` automatically when it
finds the binary on PATH.

## When to file something here

If a chat with a coding LLM (Claude Code, Codex, Cursor) produced
useful insight — UI quirks, prompt improvements, provider quirks,
strategy ideas — file it as `wiki/concepts/<short-name>.md` and link
it from the relevant entity page. The LLM-wiki pattern only works if
explorations compound.

## See also

- [AGENTS.md schema](../AGENTS.md)
- [Quiz game](../games/quiz.md)
- [Karpathy gist](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
- [qmd-rust](https://github.com/simonellefsen/qmd-rust)
