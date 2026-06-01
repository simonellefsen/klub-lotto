# AGENTS.md — klub-lotto wiki schema

This file is the **schema** for the klub-lotto LLM Wiki (see Karpathy's
[LLM Wiki](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
pattern). When a coding LLM works in this repo it should treat this file as
the authoritative description of how the wiki is structured, what
conventions to follow, and which workflows to run.

The wiki accumulates everything the project learns about Klub Lotto: the
games, their UIs, what's worked, what hasn't, and per-day source pages from
every solver run. It is meant to be read in Obsidian, browsed in GitHub,
and searched with [qmd](https://github.com/simonellefsen/qmd-rust).

## Layers (per the LLM Wiki spec)

1. **Raw sources** — `wiki/sources/`. One file per ingested event (a quiz
   round, a screenshot, a clipped article). The Go program owns the
   filename convention `<kind>-YYYYMMDD-HHMMSS.md` for automated ingests;
   manual ingests via `klub-lotto wiki ingest --file <path>` keep their
   original filename. **Never edit these by hand** — treat them as
   immutable source of truth.

2. **Wiki pages** — `wiki/games/`, `wiki/concepts/`. LLM-maintained
   markdown that synthesises the sources. `games/quiz.md` is the canonical
   entity page for the Quiz game and is regenerated from sources on every
   ingest. `concepts/` is hand- and LLM-curated mixture covering
   provider notes, agent-browser tips, etc.

3. **Schema (this file)** — read it before any wiki operation.

## Conventions

- All page filenames are lowercase kebab-case.
- All pages start with YAML frontmatter:
  ```yaml
  ---
  kind: entity | concept | source | log
  tags: [klublotto, ...]
  updated: <RFC3339 timestamp, optional for sources>
  ---
  ```
- Cross-references are relative markdown links (`[Quiz](../games/quiz.md)`)
  so the wiki renders correctly in GitHub and Obsidian.
- Statistics tables in entity pages are recomputed from sources, not
  edited by hand. The Go program does this on each ingest.
- Use Danish for question/answer text reproduced from the site;
  use English for analysis and commentary unless the user prefers
  otherwise.

## Special files

- `wiki/index.md` — content-oriented catalog. Auto-regenerated on every
  ingest. Don't edit by hand.
- `wiki/log.md` — chronological append-only log. The Go program writes
  one section per ingest/query/lint:
  ```
  ## [YYYY-MM-DD HH:MM UTC] ingest | quiz | <truncated subject> | outcome=<...>
  ```
  This format is grep-friendly: `grep '^## \[' wiki/log.md | tail -10`.
- `wiki/daily/YYYY-MM-DD.md` — daily answer ledger. Keep one row per game
  solved or inspected that day, including the prompt/clue, answer, whether
  it was submitted through the parent Danske Spil page, and whether the
  overview showed a checkmark.

## Workflows

### Ingest (automatic)

Every `klub-lotto quiz` run ingests a new source page automatically.
You don't have to do anything — but it's worth opening the new file
after each run to confirm the question scraping worked.

### Ingest (manual)

```bash
klub-lotto wiki ingest --file path/to/article.md
```

Copies the file into `wiki/sources/` and logs the action. If the file is
about something we should track over time (a new game variant, a UI
change), follow up by editing the relevant entity page in `wiki/games/`
or creating one.

### Query

```bash
klub-lotto wiki query "what does the quiz UI look like when there is no question today"
```

Uses `qmd` if installed (hybrid BM25 + LLM reranking once embeddings are
available); otherwise falls back to recursive `grep`. Setup once:

```bash
qmd collection add wiki/ --name klublotto
qmd context add qmd://klublotto "Klub Lotto LLM wiki — games, sources, concepts"
```

### Lint

```bash
klub-lotto wiki lint
```

Planned checks (not all implemented yet):
- Orphan source pages (no inbound links from any entity page).
- Stale entity pages (older than 30 days while sources continue to land).
- Contradictions: two source pages whose outcomes disagree about the
  same question.
- Missing pages: an option text that appears in multiple sources but has
  no concept page yet.

When working with a coding LLM, ask it to do a deeper lint pass by hand:
"open the latest 10 sources in `wiki/sources/`, compare against
`games/quiz.md`, and propose updates."

## Working with a coding LLM in this repo

Tell the LLM:

1. Read this file first.
2. Read `wiki/index.md` to see what already exists.
3. For ingests: read the source, propose a summary, ask the user, then
   apply edits to the relevant entity pages.
4. For queries: search the wiki first; only fall back to the raw sources
   or the web if the wiki has gaps. File interesting answers back as new
   pages under `wiki/concepts/`.

The point of the LLM-wiki pattern is **compound knowledge**: every run
makes the next one slightly smarter. Don't let answers evaporate into chat
scrollback — file them.
