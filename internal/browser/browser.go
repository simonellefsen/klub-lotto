// Package browser is a thin Go wrapper around the agent-browser CLI.
//
// We invoke `agent-browser` as a subprocess and parse its --json output.
// The daemon model means commands within one session reuse the same browser,
// so a Quiz run is just a sequence of Open/Snapshot/Click/Fill calls.
//
// Why not bind to CDP directly? The agent-browser team already solved the
// hard problems — accessibility-tree snapshots with stable @eN refs, session
// state encryption, auth vault. Reimplementing that in Go is not in scope.
package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client invokes agent-browser. All methods are blocking.
type Client struct {
	// Binary is the path to the agent-browser executable. If empty,
	// "agent-browser" is resolved from PATH.
	Binary string

	// Session is passed as both --session and --session-name. --session selects
	// the isolated live browser session; --session-name keeps persisted state
	// aligned on agent-browser versions that treat those as separate concepts.
	Session string

	// Headed shows the browser window. Useful while developing; in
	// production we'd run headless on the k8s node.
	Headed bool

	// DefaultTimeout caps each subprocess call.
	DefaultTimeout time.Duration
}

// New builds a Client with sensible defaults.
// The binary is resolved from AGENT_BROWSER_BIN env var if set, else "agent-browser" on PATH.
func New(session string, headed bool) *Client {
	bin := os.Getenv("AGENT_BROWSER_BIN")
	if bin == "" {
		bin = "agent-browser"
	}
	return &Client{
		Binary:         bin,
		Session:        session,
		Headed:         headed,
		DefaultTimeout: 60 * time.Second,
	}
}

// baseArgs are the flags that go on every invocation: --json for parseable
// output, --session for state persistence, and optionally --headed.
func (c *Client) baseArgs() []string {
	args := []string{"--json"}
	if c.Session != "" {
		// agent-browser's current public flag is --session. We also pass the
		// older --session-name alias because some local notes/scripts still use
		// it, and current releases harmlessly accept it after --session.
		args = append(args, "--session", c.Session, "--session-name", c.Session)
	}
	if c.Headed {
		args = append(args, "--headed")
	}
	return args
}

// rawResponse is the envelope every agent-browser command returns when
// invoked with --json. We only look at Success/Error/Data; everything else
// stays in Data and is decoded on demand by the specific call.
type rawResponse struct {
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// run executes one agent-browser command and returns the parsed envelope.
// stdout and stderr are captured and folded into the error message on non-zero
// exit; agent-browser often prints useful JSON to stdout even when the process
// returns a failure status.
func (c *Client) run(ctx context.Context, args ...string) (*rawResponse, error) {
	if c.DefaultTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.DefaultTimeout)
		defer cancel()
	}

	bin := c.Binary
	if bin == "" {
		bin = "agent-browser"
	}
	full := append(c.baseArgs(), args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("agent-browser %s: %w (stdout=%s stderr=%s)",
			strings.Join(full, " "), err, truncate(strings.TrimSpace(stdout.String()), 1000), truncate(strings.TrimSpace(stderr.String()), 1000))
	}

	out := stdout.Bytes()
	if len(out) == 0 {
		return &rawResponse{Success: true}, nil
	}
	var r rawResponse
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("parse agent-browser json: %w (raw=%s)", err, truncate(string(out), 500))
	}
	if !r.Success {
		return &r, fmt.Errorf("agent-browser reported failure: %s", r.Error)
	}
	return &r, nil
}

// Open navigates to the given URL.
func (c *Client) Open(ctx context.Context, url string) error {
	_, err := c.run(ctx, "open", url)
	return err
}

// openSettleTimeout caps how long OpenSettled blocks on a navigation.
const openSettleTimeout = 8 * time.Second

// OpenSettled navigates to url but caps how long it blocks waiting for the page
// to finish loading. danskespil keeps tracker/analytics connections open, so a
// plain Open (which waits for the page to settle) can block 15-30s even though
// the page is visually ready and interactable within a few seconds. A timeout
// here is non-fatal: the navigation continues in the browser daemon and the
// downstream snapshot / readiness retries handle the rest. It returns an error
// only for a genuine open failure (not the cap) or a parent-ctx cancellation.
func (c *Client) OpenSettled(ctx context.Context, url string) error {
	openCtx, cancel := context.WithTimeout(ctx, openSettleTimeout)
	defer cancel()
	err := c.Open(openCtx, url)
	if ctx.Err() != nil {
		return ctx.Err() // parent cancelled (e.g. Ctrl-C) — propagate
	}
	if err != nil && openCtx.Err() == nil {
		return err // genuine open failure, not our cap firing
	}
	return nil
}

// Close shuts down the browser for this session.
func (c *Client) Close(ctx context.Context) error {
	_, err := c.run(ctx, "close")
	return err
}

// URL returns the current page URL.
func (c *Client) URL(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "get", "url")
	if err != nil {
		return "", err
	}
	return decodeString(r.Data)
}

// Title returns the current page title.
func (c *Client) Title(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "get", "title")
	if err != nil {
		return "", err
	}
	return decodeString(r.Data)
}

// Click clicks the given selector or @eN ref.
func (c *Client) Click(ctx context.Context, sel string) error {
	_, err := c.run(ctx, "click", sel)
	return err
}

// Fill clears the input at sel and types value into it.
func (c *Client) Fill(ctx context.Context, sel, value string) error {
	_, err := c.run(ctx, "fill", sel, value)
	return err
}

// Type fires real keystrokes into sel (preserves keydown/keyup events,
// matters for some Single-Page Apps that watch for them).
func (c *Client) Type(ctx context.Context, sel, value string) error {
	_, err := c.run(ctx, "type", sel, value)
	return err
}

// KeyboardType types the given text using real keystrokes at the host/automation level
// (the "keyboard type" command, no selector required). Useful after a MouseClick focus
// inside a cross-origin game iframe (e.g. ordknude virtual kb) to send letters including
// ÆØÅ directly to the embedded game's input row.
func (c *Client) KeyboardType(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	_, err := c.run(ctx, "keyboard", "type", text)
	return err
}

// Frame switches the agent-browser session to operate inside the iframe matched
// by the CSS selector. After calling Frame, subsequent commands (snapshot, click,
// eval, press…) run in the context of that iframe. Call Frame("") or Frame("main")
// to return to the top-level page frame.
func (c *Client) Frame(ctx context.Context, selector string) error {
	_, err := c.run(ctx, "frame", selector)
	return err
}

// Press fires a single key press (Enter, Escape, etc).
func (c *Client) Press(ctx context.Context, key string) error {
	_, err := c.run(ctx, "press", key)
	return err
}

// MouseMove moves the pointer to absolute page coordinates without pressing.
func (c *Client) MouseMove(ctx context.Context, x, y int) error {
	_, err := c.run(ctx, "mouse", "move", strconv.Itoa(x), strconv.Itoa(y))
	return err
}

// MouseDown presses the (left) mouse button at the current pointer position.
func (c *Client) MouseDown(ctx context.Context) error {
	_, err := c.run(ctx, "mouse", "down")
	return err
}

// MouseUp releases the (left) mouse button at the current pointer position.
func (c *Client) MouseUp(ctx context.Context) error {
	_, err := c.run(ctx, "mouse", "up")
	return err
}

// MouseClick clicks absolute page coordinates using real mouse events.
// This is useful for canvas/custom widgets where CSS selectors are unstable.
func (c *Client) MouseClick(ctx context.Context, x, y int) error {
	if _, err := c.run(ctx, "mouse", "move", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		return err
	}
	if _, err := c.run(ctx, "mouse", "down"); err != nil {
		return err
	}
	time.Sleep(35 * time.Millisecond)
	_, err := c.run(ctx, "mouse", "up")
	return err
}

// WaitForLoad blocks until the page reaches the given load state.
// Valid states: "load", "domcontentloaded", "networkidle".
func (c *Client) WaitForLoad(ctx context.Context, state string) error {
	_, err := c.run(ctx, "wait", "--load", state)
	return err
}

// settleTimeout caps a "networkidle" wait. danskespil keeps tracker/analytics
// connections open, so a raw networkidle wait can block for ~30s (a very
// noticeable stall) before the page is interactable.
const settleTimeout = 6 * time.Second

// WaitSettled waits for the page to reach "networkidle", but caps the wait at
// settleTimeout so a tracker-heavy page can't stall the run. It returns early
// when the page genuinely settles; the wait is best-effort (downstream snapshot/
// readiness retries cover anything not yet painted), so there is no error to
// return.
func (c *Client) WaitSettled(ctx context.Context) {
	waitCtx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	_ = c.WaitForLoad(waitCtx, "networkidle")
}

// WaitForText blocks until text appears anywhere on the page.
func (c *Client) WaitForText(ctx context.Context, text string) error {
	_, err := c.run(ctx, "wait", "--text", text)
	return err
}

// WaitForSelector blocks until selector is visible.
func (c *Client) WaitForSelector(ctx context.Context, sel string) error {
	_, err := c.run(ctx, "wait", sel)
	return err
}

// SnapshotInteractive returns the page's interactive elements as plain
// text in the format agent-browser emits (one line per element with @eN
// refs). It's the form LLMs are best at parsing.
//
// Example:
//
//   - button "Log ind" [ref=e1]
//   - textbox "Email" [ref=e2]
func (c *Client) SnapshotInteractive(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "snapshot", "-i")
	if err != nil {
		return "", err
	}
	// agent-browser snapshot returns { "snapshot": "...", "refs": {...} }.
	var payload struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		// Some versions return the snapshot as a raw string.
		return decodeString(r.Data)
	}
	return payload.Snapshot, nil
}

// SnapshotInteractiveCursor is like SnapshotInteractive but also includes
// cursor-interactive elements (those with [cursor:pointer], [onclick],
// tabindex etc. that are not in the normal interactive tree).
//
// These are often the true click targets for custom components (e.g. quiz
// answer cards that are divs with cursor:pointer + click handlers).
//
// Use this (or a fresh snapshot with -C inside the relevant frame) before
// clicking quiz answers or other heavily customized UI.
func (c *Client) SnapshotInteractiveCursor(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "snapshot", "-i", "-C")
	if err != nil {
		return "", err
	}
	var payload struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return decodeString(r.Data)
	}
	return payload.Snapshot, nil
}

// SnapshotInteractiveWithFrames is like SnapshotInteractive but passes -F
// so that content inside embedded iframes (e.g. the immerspiele game launcher
// "SPIL ORDKLØVER" button) appears in the snapshot tree under
// "# [iframe: kl-game__iframe]" sections. This lets FindRefByName locate
// launcher buttons from the parent page snapshot without an explicit Frame()
// switch (the keyboard buttons inside the running game still require Frame()
// for reliable refs and clicks).
func (c *Client) SnapshotInteractiveWithFrames(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "snapshot", "-i", "-F")
	if err != nil {
		return "", err
	}
	var payload struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return decodeString(r.Data)
	}
	return payload.Snapshot, nil
}

// SnapshotWithFrames returns the full (non-interactive) accessibility snapshot
// including cross-frame content. Unlike SnapshotInteractiveWithFrames it does
// not filter to interactive-only nodes, so the iframe's text nodes (e.g. the
// "- text: s p a l t ø r k e n …" board text in Ordknude) are included.
// This lets us read the board letters from the parent page without switching
// frame context via Frame() — which fails for cross-origin iframes.
func (c *Client) SnapshotWithFrames(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "snapshot", "-F")
	if err != nil {
		return "", err
	}
	var payload struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return decodeString(r.Data)
	}
	return payload.Snapshot, nil
}

// Snapshot returns the full accessibility snapshot when agent-browser
// supports it. It is useful for success pages where static text matters more
// than clickable refs.
func (c *Client) Snapshot(ctx context.Context) (string, error) {
	r, err := c.run(ctx, "snapshot")
	if err != nil {
		return "", err
	}
	var payload struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return decodeString(r.Data)
	}
	return payload.Snapshot, nil
}

// GetText returns the textContent of the matched element.
func (c *Client) GetText(ctx context.Context, sel string) (string, error) {
	r, err := c.run(ctx, "get", "text", sel)
	if err != nil {
		return "", err
	}
	return decodeString(r.Data)
}

// Screenshot saves a PNG to path (relative to the agent-browser cwd or
// absolute). Used liberally during PoC for after-the-fact debugging.
func (c *Client) Screenshot(ctx context.Context, path string) error {
	_, err := c.run(ctx, "screenshot", path)
	return err
}

// Eval runs JavaScript in the page and returns its result. Useful when the
// accessibility tree doesn't carry the information we need (custom widgets,
// CSS-only state).
func (c *Client) Eval(ctx context.Context, js string) (string, error) {
	r, err := c.run(ctx, "eval", js)
	if err != nil {
		return "", err
	}
	return decodeString(r.Data)
}

// IsVisible reports whether sel exists in the layout tree and is visible.
func (c *Client) IsVisible(ctx context.Context, sel string) (bool, error) {
	r, err := c.run(ctx, "is", "visible", sel)
	if err != nil {
		return false, err
	}
	var b bool
	if err := json.Unmarshal(r.Data, &b); err == nil {
		return b, nil
	}
	// Some versions wrap it.
	var wrap struct {
		Visible bool `json:"visible"`
	}
	if err := json.Unmarshal(r.Data, &wrap); err == nil {
		return wrap.Visible, nil
	}
	return false, fmt.Errorf("could not parse is-visible response: %s", string(r.Data))
}

func decodeString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	// Try as JSON string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// Try common wrapped string fields. Current agent-browser eval responses
	// use {"origin":"...","result":"..."}.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, k := range []string{"result", "value", "text", "url", "title"} {
			if v, ok := obj[k].(string); ok {
				return v, nil
			}
		}
	}
	return string(raw), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Discard is io.Discard; re-exported so callers don't need to import io
// just for it.
var Discard io.Writer = io.Discard
