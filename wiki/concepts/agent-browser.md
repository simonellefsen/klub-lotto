---
kind: concept
tags: [klublotto, automation, agent-browser]
updated: 2026-06-06T00:00:00Z
---

# agent-browser

We drive the browser via [vercel-labs/agent-browser](https://github.com/vercel-labs/agent-browser):
native Rust CLI, daemon-backed, accessibility-tree snapshots with stable
`@eN` / `[ref=eN]` element refs. The Go wrapper lives in `internal/browser/`.

## Why this and not Playwright/Selenium

- Snapshot+ref workflow is purpose-built for LLM-driven flows.
- Sessions persist cleanly across runs (`--session klublotto --session-name klublotto`).
- One CLI binary, no Node toolchain.

---

## Session model

We set both `AGENT_BROWSER_SESSION=klublotto` and
`AGENT_BROWSER_SESSION_NAME=klublotto` in k8s. `AGENT_BROWSER_SESSION`
selects the isolated live browser session; `AGENT_BROWSER_SESSION_NAME`
selects the auto-save/load persistence name. Keeping them equal means the
VNC browser and automation commands operate on the same session and persisted
state. Encryption at rest if you set `AGENT_BROWSER_ENCRYPTION_KEY` to a
64-char hex string — recommended in production:

```bash
export AGENT_BROWSER_ENCRYPTION_KEY=$(openssl rand -hex 32)
```

## Headed vs headless

K8s default is **headed** so the operator can watch and intervene through VNC.
Do not mix headed and headless calls against the same session; this has caused
flaky browser/session behavior. The pod sets both `KLUBLOTTO_HEADED=true` and
`AGENT_BROWSER_HEADED=true`, and every web job plus auth probe should use the
visible VNC-backed `klublotto` session.

---

## Command reference

All commands take `--json` for machine-parseable output, and `--session` /
`--session-name` to target a specific browser session.

### Navigation

```bash
agent-browser open <url>                 # navigate to URL
agent-browser wait --load networkidle    # wait for network quiet
agent-browser wait --load domcontentloaded
agent-browser wait --text "some text"   # wait until text appears
agent-browser wait <css-selector>       # wait until selector visible
```

### Interaction

```bash
agent-browser click <ref-or-selector>   # click element (e.g. @e2 or [ref=e5])
agent-browser fill <sel> <value>        # clear + type into input
agent-browser type <sel> <value>        # real keystrokes into element
agent-browser press <key>               # single key (Enter, Escape, Tab, …)
agent-browser keyboard type <text>      # OS-level keystrokes, no selector needed
                                         # (works inside cross-origin iframes)
agent-browser mouse move <x> <y>
agent-browser mouse down
agent-browser mouse up
```

`keyboard type` is useful when the game has already focused its own input
inside a cross-origin iframe: send letters including ÆØÅ directly without
needing an element ref.

### Reading page state

```bash
agent-browser get url
agent-browser get title
agent-browser get text <css-selector>
agent-browser is visible <css-selector>   # returns {"visible": true/false}
agent-browser eval '<js>'                 # run JS, returns result JSON
```

### Screenshots

```bash
agent-browser screenshot /tmp/foo.png    # save PNG (absolute or relative path)
```

### Frames

```bash
agent-browser frame <css-selector>       # switch context into an iframe
agent-browser frame ""                   # return to top-level frame
```

> **⚠️ `frame` almost always fails in practice.** All selectors tried —
> `iframe.kl-game__iframe`, `iframe[src*='ordknude']`, plain `iframe`, even
> dynamic full URLs — return `{"error":"Frame not found"}`. Workaround: use
> `snapshot` (bare, no flags) from the parent page — iframe content is already
> in the base accessibility tree when the iframe is same-origin or when
> agent-browser's Chromium bridge embeds it automatically. See
> [Snapshots → iframe section](#iframes-and-the-snapshot-tree) below.

---

## Snapshots — the core concept

A snapshot is a text dump of the accessibility tree. Each node is one line with
a type, optional name/label, and a stable `[ref=eN]` ref that can be passed to
`click`, `fill`, etc. Refs are valid only for the lifetime of that snapshot;
re-snapshot after any navigation or significant DOM change.

### `snapshot` flag matrix

Tested empirically on `danskespil.dk/klublotto/dagens-ordknuden` (2026-06-06).
Results may differ on other pages.

| Command | Output | Use case |
|---|---|---|
| `snapshot` | Full accessibility tree, ~364 lines. Includes StaticText, all roles. | Reading text content, board letters, page state |
| `snapshot -C` | **Identical to bare `snapshot` on this page.** (`-C` = include cursor-interactive elements — already present in the base tree here) | — |
| `snapshot -F` | **Identical to bare `snapshot` on this page.** (`-F` = include frame content — already embedded in the tree here) | — |
| `snapshot -C -F` | **Identical to bare `snapshot` on this page.** Both flags redundant | — |
| `snapshot -i` | Interactive-only filter: buttons, links, inputs. ~80 lines. **StaticText nodes are omitted.** | Clicking: finding `[ref=eN]` for buttons |
| `snapshot -i -C` | Adds cursor-interactive elements (divs with `onclick`/pointer cursor) to `-i` output. | Quiz answer cards, custom widgets |
| `snapshot -i -F` | **Same as `-i`.** The `-F` flag is a no-op when combined with `-i` on this page. | — |
| `snapshot -i -C -F` | **Same as `-i`.** All extra flags redundant with `-i`. | — |

**Rule of thumb:**
- Need to **click something**? Use `snapshot -i` (fast, small, only interactive refs).
- Need to **read text / board state**? Use bare `snapshot` (full tree with StaticText).
- Custom UI with pointer-cursor divs not in `-i`? Add `-C`: `snapshot -i -C`.

### Iframes and the snapshot tree

On pages with embedded iframes, bare `snapshot` already includes the iframe
content at the **direct-child indentation level** of the `- Iframe` node:

```
- Iframe [ref=e52]
  - StaticText "Ø"      ← tile letters, 2-space indent from Iframe
  - StaticText "R"
  - StaticText "K"
  - StaticText "E"
  - StaticText "N"
  - StaticText "S"
  - StaticText "P"
  …
  - generic             ← keyboard container; marks end of tiles
    - button "Q" [ref=e60]
    - button "W" [ref=e61]
    …
```

Key facts:
- **5 letters per guessed word** appear as consecutive `StaticText "X"` nodes.
- The **`generic` container** (keyboard) is a direct child of `Iframe` at the
  same indent level as the letter nodes — stop reading there.
- `StaticText` nodes do **not** appear with `-i` — you must use bare `snapshot`
  to read them.
- `snapshot -F` and `snapshot -C` give identical output to bare `snapshot` here
  because the iframe is already embedded in the base tree. The `-F` flag would
  only add value on pages where the iframe is genuinely cross-origin and
  excluded from the base tree by default.

### Reading snapshot output with `--json`

The `--json` flag wraps everything in:

```json
{"success": true, "data": {"snapshot": "- document\n  - main\n    ..."}}
```

Parse `.data.snapshot` for the accessibility tree string. Some older versions
return `.data` as a raw string (not an object) — handle both.

### Finding refs by name

Use `klublotto.FindRefByName(snap, []string{"Submit", "Indsend"})` (our Go
helper) or grep the snapshot text for `[ref=eN]` adjacent to the target name.
Refs look like `[ref=e42]` and can be passed verbatim to `click`.

---

## Core workflow

```bash
# Navigate
agent-browser --json --session klublotto open https://danskespil.dk/klublotto

# Read board state (StaticText letters, full tree)
agent-browser --json --session klublotto snapshot

# Find interactive refs (buttons, links)
agent-browser --json --session klublotto snapshot -i

# Click a button by ref
agent-browser --json --session klublotto click @e42

# Type into a game iframe (keyboard-level, no selector needed)
agent-browser --json --session klublotto keyboard type "HÆDER"

# Submit with Enter
agent-browser --json --session klublotto press Enter

# Screenshot for debugging / vision
agent-browser --json --session klublotto screenshot /tmp/board.png
```

---

## Selectors

In rough order of preference:

1. **`[ref=eN]` refs** from a fresh `snapshot -i`. Stable for the lifetime of
   the snapshot. Pass as `@eN` or `[ref=eN]` — both work.
2. **`find role <role> --name "..."`** and `find label`. Survives UI churn
   better than CSS.
3. **CSS** as a last resort. The `klublotto.fillByAnyOf` helper passes several
   CSS candidates so we don't depend on any single class name.

---

## Cross-origin iframes

Klub Lotto games hosted by Immer Spiele live in cross-origin iframes. The
accessibility tree is exposed automatically via the base `snapshot` — no frame
switching needed for reading. For **clicking** inside the iframe:

- Interactive buttons (keyboard keys) appear in the `generic` keyboard container
  as `button "Q" [ref=eN]` nodes — use refs directly with `click`.
- If `frame` switching ever works, clicking returns to normal. In practice,
  use the pre-built ref map approach (snapshot once, map all letter refs,
  click each without re-snapshotting).
- For canvas or non-accessible widgets: use `mouse move/down/up` with absolute
  coordinates obtained from `eval('el.getBoundingClientRect()')`.

Do **not** open the Immer Spiele iframe URL as a top-level page. The iframe
posts completion events to `window.parent`, so the Danske Spil page must be the
parent for earned tickets to register.

---

## Anti-bot considerations

Danske Spil is a regulated gambling site and may run bot-detection. The
agent-browser daemon already uses a real Chrome (not headless-flagged
unless we ask for it), so for headed runs we're indistinguishable from
a user. If detection lands, the next moves are:

1. `--profile "Default"` to ride on the user's actual Chrome profile.
2. Run via `--proxy` from a residential IP.
3. Use Kernel/Browserbase providers with stealth mode.

---

## Danish characters (ÆØÅ)

`keyboard type` sends OS-level keystrokes and handles ÆØÅ correctly. The
virtual on-screen keyboard buttons for Æ, Ø, Å appear in the snapshot as
`button "Æ" [ref=eN]` etc. — click them by ref rather than trying CSS
`#key-ae` selectors.

**Go byte vs rune**: `len("ØRKEN") == 6` (bytes), not 5. Always use
`len([]rune(word))` when checking Danish word lengths. Getting this wrong
silently drops every word containing Ø, Æ, or Å.

---

## Go wrapper (`internal/browser/`)

| Method | Underlying command |
|---|---|
| `br.Open(ctx, url)` | `open <url>` |
| `br.Snapshot(ctx)` | `snapshot` (full tree) |
| `br.SnapshotInteractive(ctx)` | `snapshot -i` |
| `br.SnapshotInteractiveCursor(ctx)` | `snapshot -i -C` |
| `br.SnapshotInteractiveWithFrames(ctx)` | `snapshot -i -F` |
| `br.SnapshotWithFrames(ctx)` | `snapshot -F` (≡ bare snapshot on this site) |
| `br.Click(ctx, ref)` | `click <ref>` |
| `br.Fill(ctx, sel, val)` | `fill <sel> <val>` |
| `br.Type(ctx, sel, val)` | `type <sel> <val>` |
| `br.KeyboardType(ctx, text)` | `keyboard type <text>` |
| `br.Press(ctx, key)` | `press <key>` |
| `br.Frame(ctx, sel)` | `frame <sel>` (⚠️ often fails — see above) |
| `br.MouseClick(ctx, x, y)` | `mouse move/down/up` sequence |
| `br.Eval(ctx, js)` | `eval '<js>'` |
| `br.Screenshot(ctx, path)` | `screenshot <path>` |
| `br.WaitForLoad(ctx, state)` | `wait --load <state>` |
| `br.WaitForText(ctx, text)` | `wait --text <text>` |

---

## Debugging checklist

1. **Nothing found in snapshot?** Check if a welcome/splash screen is showing.
   Board letters only appear in the accessibility tree after the game starts.
   Call `startWordGameIfPresent` first, wait ~1200 ms, retry snapshot.

2. **`frame` returns "Frame not found"?** Don't fight it. Use bare `snapshot`
   from the parent page instead — the iframe content is already there.

3. **StaticText missing from snapshot?** You used `snapshot -i`. Switch to bare
   `snapshot` for reading text content.

4. **Vision misreading tile colors?** The small dark-maroon absent tiles are
   easily confused with the dark red tile background. Prefer reading the
   **keyboard** instead: keyboard keys are large, and green/yellow colors are
   unmistakable. S=green means S is "correct"; L=yellow means L is "present".

5. **Vision returns JSON instead of the requested plain-text format?** Strip
   quotes from parsed word keys (`strings.Trim(word, "\"'")`) so matching
   against the known word list still works.

6. **Stale binary?** Always `make build` before re-testing. `go build ./...`
   succeeds even if the `bin/` binary is stale.

---

## See also

- [Immer Spiele embeds](immerspiele.md)
- [LLM providers](llm-providers.md)
