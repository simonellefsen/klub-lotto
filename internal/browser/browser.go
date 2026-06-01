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
	"os/exec"
	"strings"
	"time"
)

// Client invokes agent-browser. All methods are blocking.
type Client struct {
	// Binary is the path to the agent-browser executable. If empty,
	// "agent-browser" is resolved from PATH.
	Binary string

	// Session is passed as --session-name. Cookies + localStorage persist
	// under ~/.agent-browser/sessions/<name>/ across runs, so a successful
	// login carries over to the next invocation.
	Session string

	// Headed shows the browser window. Useful while developing; in
	// production we'd run headless on the k8s node.
	Headed bool

	// DefaultTimeout caps each subprocess call.
	DefaultTimeout time.Duration
}

// New builds a Client with sensible defaults.
func New(session string, headed bool) *Client {
	return &Client{
		Binary:         "agent-browser",
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

// Press fires a single key press (Enter, Escape, etc).
func (c *Client) Press(ctx context.Context, key string) error {
	_, err := c.run(ctx, "press", key)
	return err
}

// WaitForLoad blocks until the page reaches the given load state.
// Valid states: "load", "domcontentloaded", "networkidle".
func (c *Client) WaitForLoad(ctx context.Context, state string) error {
	_, err := c.run(ctx, "wait", "--load", state)
	return err
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
	// Try {"value":"..."} or {"text":"..."}.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, k := range []string{"value", "text", "url", "title"} {
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
