package klublotto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// KaedeordPoint is a click point in the game frame's viewport (CSS px).
type KaedeordPoint struct {
	X, Y float64
}

// KaedeordLive is one read of the game frame.
type KaedeordLive struct {
	Screen   string // "board", "welcome", "confirm", "done", "answered" or "loading"
	Board    KaedeordBoard
	Score    int                   // the live "Point" counter; -1 when not shown
	Deletes  int                   // delete lifelines left
	Swaps    int                   // swap lifelines left
	At       map[int]KaedeordPoint // tile ID → centre
	Classes  map[int]string        // tile ID → class list, to spot the selection
	Buttons  map[string]KaedeordPoint
	WordList string // URL of the word list the game loaded
	Text     string
	Main     string // the <main> text: where the parent page puts the result once the game frame is gone
}

// kaedeordReadJS reads the game frame in one go. Tiles are div[data-tile-id]
// inside a CSS grid of Rows×Cols cells laid out row by row, so a tile's cell
// index gives its grid position. The lifeline buttons sit before "Afslut" and
// show their remaining count (blank at 0).
const kaedeordReadJS = `(() => {
  const text = (document.body && document.body.innerText) || "";
  const main = document.querySelector("main");
  const out = {text: text.slice(0, 3000), main: main ? (main.innerText || "").slice(0, 600) : "", tiles: [], buttons: {}, score: -1, deletes: 0, swaps: 0};
  out.words = performance.getEntriesByType("resource").map(e => e.name).filter(n => /\/fallout\/words-[^\/?#]+\.txt/.test(n));
  const centre = el => { const r = el.getBoundingClientRect(); return {x: r.x + r.width / 2, y: r.y + r.height / 2}; };
  for (const b of document.querySelectorAll("button")) {
    const t = (b.innerText || "").trim().toLowerCase();
    if (t === "spil kædeord") out.buttons.start = centre(b);
    if (t === "afslut") out.buttons.end = centre(b);
    if (t === "bekræft") out.buttons.confirm = centre(b);
    if (t === "spil videre") out.buttons.resume = centre(b);
  }
  const end = [...document.querySelectorAll("button")].find(b => (b.innerText || "").trim().toLowerCase() === "afslut");
  if (end && end.parentElement && end.parentElement.parentElement) {
    const ops = [...end.parentElement.parentElement.firstElementChild.querySelectorAll("button")];
    const del = ops.find(b => b.querySelector("svg line") && !b.querySelector("svg polyline"));
    const swap = ops.find(b => b.querySelector("svg polyline"));
    const count = b => parseInt((b.innerText || "").trim(), 10) || 0;
    if (del) { out.buttons.delete = centre(del); out.deletes = count(del); }
    if (swap) { out.buttons.swap = centre(swap); out.swaps = count(swap); }
  }
  const grid = [...document.querySelectorAll("div")].find(d => /repeat\(/.test(d.style.gridTemplateColumns || ""));
  if (grid) {
    out.cols = +((grid.style.gridTemplateColumns.match(/repeat\((\d+)/) || [])[1] || 0);
    out.rows = +((grid.style.gridTemplateRows.match(/repeat\((\d+)/) || [])[1] || 0);
    [...grid.children].forEach((cell, i) => {
      const t = cell.querySelector("[data-tile-id]");
      if (!t || !out.cols) return;
      const letter = [...t.childNodes].find(n => n.nodeType === 3 && n.textContent.trim());
      const m = (t.innerText.match(/×\s*(\d+)/) || [])[1];
      const c = centre(t);
      out.tiles.push({id: +t.dataset.tileId, x: i % out.cols, y: Math.floor(i / out.cols),
        l: letter ? letter.textContent.trim() : "", m: m ? +m : 1, cx: c.x, cy: c.y, c: String(t.className)});
    });
    const label = [...document.querySelectorAll("p")].find(p => p.innerText.trim() === "Point");
    if (label) out.score = parseInt(label.parentElement.innerText, 10);
  }
  return JSON.stringify(out);
})()`

// ReadKaedeordLive reads the game. The browser must already be in the game
// frame (EnterGameFrame).
func ReadKaedeordLive(ctx context.Context, br *browser.Client) (KaedeordLive, error) {
	raw, err := br.Eval(ctx, kaedeordReadJS)
	if err != nil {
		return KaedeordLive{}, err
	}
	var r struct {
		Text    string                   `json:"text"`
		Main    string                   `json:"main"`
		Words   []string                 `json:"words"`
		Cols    int                      `json:"cols"`
		Rows    int                      `json:"rows"`
		Score   int                      `json:"score"`
		Deletes int                      `json:"deletes"`
		Swaps   int                      `json:"swaps"`
		Buttons map[string]KaedeordPoint `json:"buttons"`
		Tiles   []struct {
			ID     int     `json:"id"`
			X      int     `json:"x"`
			Y      int     `json:"y"`
			Letter string  `json:"l"`
			Mult   int     `json:"m"`
			CX     float64 `json:"cx"`
			CY     float64 `json:"cy"`
			Class  string  `json:"c"`
		} `json:"tiles"`
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return KaedeordLive{}, fmt.Errorf("read kædeord frame: %w (raw=%.200s)", err, raw)
	}
	live := KaedeordLive{
		Score: r.Score, Deletes: r.Deletes, Swaps: r.Swaps, Buttons: r.Buttons, Text: r.Text, Main: r.Main,
		At: map[int]KaedeordPoint{}, Classes: map[int]string{},
	}
	if len(r.Words) > 0 {
		live.WordList = r.Words[len(r.Words)-1]
	}
	var tiles []KaedeordTile
	for _, t := range r.Tiles {
		rs := []rune(strings.ToUpper(t.Letter))
		if len(rs) != 1 {
			return KaedeordLive{}, fmt.Errorf("kædeord tile %d reads %q, not one letter", t.ID, t.Letter)
		}
		tiles = append(tiles, KaedeordTile{ID: t.ID, X: t.X, Y: t.Y, Letter: rs[0], Mult: t.Mult})
		live.At[t.ID] = KaedeordPoint{t.CX, t.CY}
		live.Classes[t.ID] = t.Class
	}
	if r.Rows > 0 {
		if live.Board, err = NewKaedeordBoard(tiles, r.Rows); err != nil {
			return KaedeordLive{}, err
		}
	}
	low := strings.ToLower(r.Text)
	switch {
	case strings.Contains(low, "er du sikker på, at du vil indsende"):
		live.Screen = "confirm"
	case r.Rows > 0 && len(tiles) > 0:
		live.Screen = "board"
	case IsAlreadyPlayedText(r.Text):
		live.Screen = "answered"
	// The game's own "1 lod til dig" screen, or the parent's "Du har ordet i
	// din magt! Godt klaret!" once it has taken the frame down.
	case strings.Contains(low, "lod til dig") || strings.Contains(low, "godt klaret"):
		live.Screen = "done"
	case live.Buttons["start"] != (KaedeordPoint{}):
		live.Screen = "welcome"
	default:
		live.Screen = "loading"
	}
	return live, nil
}

// Selected lists the tiles whose classes differ from a baseline read taken with
// nothing selected: the game marks selected tiles by adding classes.
func (l KaedeordLive) Selected(baseline map[int]string) []int {
	var ids []int
	for id, c := range l.Classes {
		if b, ok := baseline[id]; ok && b != c {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// EnterKaedeord takes the game from the welcome screen to the board. Like
// EnterWordGame, a game that isn't ready after 15s gets one reload via reopen
// and 30s more. It leaves the browser in the game frame. A round that's already
// finished returns ErrAlreadyPlayed.
func EnterKaedeord(ctx context.Context, br *browser.Client, reopen func(context.Context, *browser.Client) error, click func(KaedeordPoint) error) (KaedeordLive, error) {
	live, err := waitForKaedeordBoard(ctx, br, wordGameFirstWait, click)
	if err == nil || errors.Is(err, ErrAlreadyPlayed) || ctx.Err() != nil {
		return live, err
	}
	fmt.Printf("   [enter] %v — reloading the page\n", err)
	LeaveFrame(br)
	if err := reopen(ctx, br); err != nil {
		return live, fmt.Errorf("reload after slow load: %w", err)
	}
	live, err = waitForKaedeordBoard(ctx, br, wordGameRetryWait, click)
	if err != nil && !errors.Is(err, ErrAlreadyPlayed) && ctx.Err() == nil {
		return live, fmt.Errorf("%w, after one reload", err)
	}
	return live, err
}

func waitForKaedeordBoard(ctx context.Context, br *browser.Client, budget time.Duration, click func(KaedeordPoint) error) (KaedeordLive, error) {
	deadline := time.Now().Add(budget)
	last, clicks := "not attached", 0
	for {
		if err := EnterGameFrame(ctx, br); err == nil {
			live, err := ReadKaedeordLive(ctx, br)
			if err == nil {
				last = live.Screen
				switch live.Screen {
				case "board":
					return live, nil
				case "answered", "done":
					LeaveFrame(br)
					return live, fmt.Errorf("%w (game shows %q)", ErrAlreadyPlayed, firstLine(live.Text))
				case "welcome":
					if clicks < 5 {
						clicks++
						fmt.Println("   [enter] clicking SPIL KÆDEORD")
						_ = click(live.Buttons["start"])
					}
				}
			} else {
				last = err.Error()
			}
		}
		if ctx.Err() != nil {
			return KaedeordLive{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			return KaedeordLive{}, fmt.Errorf("board not ready within %s (last state: %s)", budget, last)
		}
		time.Sleep(800 * time.Millisecond)
	}
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

var kaedeordWordListName = regexp.MustCompile(`^words-[0-9A-Za-z]+\.txt$`)

// LoadKaedeordDict returns the game's word list as a trie. The list is a
// public static file named by its content hash, so it is cached in cacheDir
// under that name and only downloaded when the game switches to a new one.
// With no URL (the game didn't report one) it falls back to the newest cached
// list.
func LoadKaedeordDict(ctx context.Context, url, cacheDir string) (*KaedeordDict, string, error) {
	var file string
	if url != "" {
		name := path.Base(strings.SplitN(url, "?", 2)[0])
		if !kaedeordWordListName.MatchString(name) {
			return nil, "", fmt.Errorf("unexpected kædeord word list %q", url)
		}
		file = filepath.Join(cacheDir, "kaedeord-"+name)
		if _, err := os.Stat(file); err != nil {
			if err := downloadKaedeordWordList(ctx, url, file); err != nil {
				return nil, "", err
			}
		}
	} else {
		cached, _ := filepath.Glob(filepath.Join(cacheDir, "kaedeord-words-*.txt"))
		sort.Slice(cached, func(i, j int) bool { return modTime(cached[i]).After(modTime(cached[j])) })
		if len(cached) == 0 {
			return nil, "", fmt.Errorf("the game reported no word list and none is cached in %s", cacheDir)
		}
		file = cached[0]
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}
	d := NewKaedeordDict(ParseKaedeordWordList(string(raw)))
	if d.Words < 10000 {
		return nil, "", fmt.Errorf("kædeord word list %s decoded to only %d words", file, d.Words)
	}
	return d, file, nil
}

func modTime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

var kaedeordHTTP = &http.Client{Timeout: 60 * time.Second}

func downloadKaedeordWordList(ctx context.Context, url, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := kaedeordHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download kædeord word list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download kædeord word list: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("download kædeord word list: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".part"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
