# RUN.md — running the PoC locally

Quick guide to take the PoC from a fresh `git clone` to a working Quiz
solver on your Mac.

## 1. Prerequisites

```bash
# Go (only if you don't already have it)
brew install go

# agent-browser CLI (required)
brew install agent-browser
agent-browser install            # downloads Chrome for Testing

# qmd-rust (optional; enables better wiki search)
brew tap simonellefsen/qmd
brew install qmd
```

Verify:

```bash
go version
agent-browser --version
qmd status         # optional
```

## 2. Secrets

`.env.local` should already exist in the repo root. Confirm it has all
five values (mask the actual secrets when sharing):

```
DANSKESPIL_USERNAME=...
DANSKESPIL_PASSWORD=...
OPENAI_API_KEY=...
OPENAI_MODEL=gpt-5.4      # optional; default is gpt-5.4
XAI_API_KEY=...
GEMINI_API_KEY=...
ORDKNUDE_PROVIDER=gemini     # optional: gemini/openai/xai/anthropic/openrouter
OPENROUTER_API_KEY=...       # optional; only needed for ORDKNUDE_PROVIDER=openrouter
OPENROUTER_MODEL=...         # optional; default google/gemini-2.5-flash
```

For extra paranoia, encrypt agent-browser's local session storage:

```bash
echo "export AGENT_BROWSER_ENCRYPTION_KEY=$(openssl rand -hex 32)" >> ~/.zshrc
source ~/.zshrc
```

## 3. First-run sanity check

```bash
make build
make doctor
```

`doctor` prints which providers are configured, the agent-browser path,
and whether qmd is installed. Fix anything red before moving on.

## 4. First-time login (one-time MitID handoff)

Klub Lotto is MitID-gated, so the very first run needs you to complete
MitID by hand:

```bash
make login
```

This opens a visible Chrome window and walks to the MitID screen. You
finish MitID on your phone (or hardware token). The program polls every
2 seconds and continues as soon as it sees you're back on Klub Lotto.
You can also press Enter in the terminal to confirm manually.

After this, agent-browser saves the session cookie under
`~/.agent-browser/sessions/klublotto/`. Subsequent runs reuse it — until
Danske Spil expires the cookie (likely days-to-weeks; we'll learn the
exact lifetime as we go). When it expires, `make quiz` will detect the
redirect and run the same handoff again. See
[wiki/concepts/mitid-handoff.md](wiki/concepts/mitid-handoff.md).

## 5. The actual run

```bash
make quiz-dry      # opens a visible Chrome, reuses the session, finds the
                   # question, asks all four LLMs, prints the majority vote
                   # — does NOT click.
```

You should see:

- A Chrome window open.
- The login form get auto-filled and submitted.
- The Klub Lotto Quiz page load.
- A printed transcript of question → options → votes → majority choice.
- A new file under `wiki/sources/quiz-YYYYMMDD-HHMMSS.md`.
- `wiki/games/quiz.md` and `wiki/index.md` regenerated.
- `wiki/log.md` extended with one new section.

Watch the browser. If anything fails:

- The wiki source page is still written with the votes, so you can
  inspect the prompt and options regardless.
- A debug snapshot lands in `.klublotto/quiz-snapshot.txt`.
- Take a screenshot manually (`agent-browser --session klublotto --session-name klublotto screenshot debug.png`).

When you're happy with the dry run:

```bash
make quiz          # same flow but actually clicks the chosen answer.
```

`make quiz` also runs `scripts/sync.sh` which commits `wiki/` + docs and
pushes to `origin` if a remote is set up.

Ordknuden is not safe to dry-run because every accepted guess is stored by
Klub Lotto as game state:

```bash
make ordknude      # solves today's Ordknuden; default provider is Gemini.
```

Operational notes:

- Use real Danish dictionary words only. Good reference: ODS/ordnet.dk or
  Den Danske Ordbog. Avoid Swedish/Norwegian-looking suggestions even when
  they fit the letters.
- The answer can contain `Æ`, `Ø`, and `Å`. For Ordknuden, click the
  on-screen keyboard buttons for all letters; raw keyboard typing can drop
  Danish letters (`SALÆR` became `SALR` during testing).
- If the game says `Ordet findes ikke i vores database`, clear the partial
  row and try a different Danish dictionary candidate. Do not repeat rejected
  words.
- Accepted guesses are irreversible. Read the current persisted board first
  and solve from that state.
- The Ordknuden solver depends on the narrow `llm.JSONGenerator` interface,
  so the suggester can be swapped without changing game logic:
  `bin/klub-lotto ordknude --provider openai`,
  `--provider anthropic`, `--provider xai`, or `--provider openrouter`.
  OpenRouter uses its OpenAI-compatible `/api/v1/chat/completions` endpoint.

## 6. GitHub remote

```bash
git init -b main
git add .
git commit -m "feat: initial klub-lotto Quiz solver PoC"
git remote add origin git@github.com:simonellefsen/klub-lotto.git
git push -u origin main
```

After this, `make quiz` keeps the repo current automatically.

## 7. Wiki workflow

```bash
make wiki-query Q="what happens when there is no quiz today"

# Once-only qmd setup so the query is hybrid search rather than grep:
qmd collection add wiki/ --name klublotto
qmd context add qmd://klublotto "klub-lotto wiki — games, sources, concepts"
```

Read [`wiki/AGENTS.md`](wiki/AGENTS.md) before doing any larger edit. It
describes the schema the wiki follows (per Karpathy's LLM-Wiki pattern).

## 8. Scheduling

Once the headed PoC is reliable, run it on a schedule. Two options:

**macOS launchd (recommended for local Mac):**

```xml
<!-- ~/Library/LaunchAgents/com.simonellefsen.klublotto.quiz.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key><string>com.simonellefsen.klublotto.quiz</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>-lc</string>
    <string>cd /Users/lindau/codex/klub-lotto && make quiz --headless</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>15</integer></dict>
  <key>StandardOutPath</key><string>/tmp/klublotto.out.log</string>
  <key>StandardErrorPath</key><string>/tmp/klublotto.err.log</string>
</dict>
</plist>
```

Then: `launchctl load ~/Library/LaunchAgents/com.simonellefsen.klublotto.quiz.plist`

**k8s (docker-desktop, future):**

Once the headed run works, build a container image, install
agent-browser inside it with `--with-deps`, and run as a CronJob in the
`klub-lotto` namespace. State persists in a Postgres database managed
by cnpg (see `docs/k8s-plan.md` — not yet written).

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `agent-browser: not found` | not installed or not on PATH | `brew install agent-browser && agent-browser install` |
| Login times out after 5 min | MitID not completed in time | re-run `make login`, finish MitID before timeout |
| `quiz` hangs on first run after weeks | session cookie expired | the program will redirect you to MitID — finish it once, then continue |
| Login stalls at consent banner | cookie banner copy changed | edit `dismissCookieBanner` selectors in `internal/klublotto/login.go` |
| `extract round: could not identify quiz prompt` | site DOM changed | open `.klublotto/quiz-snapshot.txt`, adjust heuristics in `internal/klublotto/quiz.go` |
| All providers return errors | bad API key or rate limit | re-check `.env.local`; `make doctor` shows masked previews |
| `make quiz` succeeds but no answer was submitted | majority vote was -1 | check the wiki source page — at least one provider returned an unparseable response |

## See also

- [README.md](README.md) — high-level project overview
- [wiki/AGENTS.md](wiki/AGENTS.md) — wiki schema and conventions
- [wiki/concepts/agent-browser.md](wiki/concepts/agent-browser.md) — selector/session tips
- [wiki/concepts/llm-providers.md](wiki/concepts/llm-providers.md) — model defaults and Anthropic setup
