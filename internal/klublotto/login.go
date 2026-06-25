package klublotto

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

var accountBalanceRE = regexp.MustCompile(`\b\d+,\d{2}\s*kr\.`)

// Login opens danskespil.dk's login page, fills credentials, submits, and
// waits for a logged-in indicator to appear. It is idempotent: if the
// session already has a valid cookie, we short-circuit and return nil.
//
// The DanskeSpil login UI is React + their own component library; we don't
// rely on element IDs. Instead we snapshot the page and pick fields by
// label/placeholder via agent-browser's semantic locators.
func Login(ctx context.Context, br *browser.Client, username, password string) error {
	if username == "" || password == "" {
		return errors.New("klublotto: empty username or password")
	}

	// Step 1: visit Klub Lotto home so an existing session cookie can be
	// picked up. If the snapshot shows we are already authenticated we
	// don't go to the login form at all.
	if err := br.OpenSettled(ctx, KlubLottoURL); err != nil {
		return fmt.Errorf("open klublotto: %w", err)
	}

	if loggedIn, _ := IsLoggedIn(ctx, br); loggedIn {
		return nil
	}

	// Step 2: navigate to the login form.
	if err := br.OpenSettled(ctx, LoginURL); err != nil {
		return fmt.Errorf("open login: %w", err)
	}

	// Step 3: cookie/consent banner often blocks the form on first run.
	// We try a few common accept buttons; failures are non-fatal.
	tryClickFirst(ctx, br,
		"button:has-text('Accepter alle')",
		"button:has-text('Acceptér alle')",
		"button:has-text('Accept all')",
		"#declineButton", // some flows hide accept under a chooser
	)

	// Step 4: fill the username field. Danske Spil labels it
	// "Brugernavn" or "Email/Brugernavn"; both share a placeholder.
	if err := fillByAnyOf(ctx, br, username,
		"input[name=username]",
		"input[name=email]",
		"input[type=text]",
	); err != nil {
		return fmt.Errorf("fill username: %w", err)
	}

	if err := fillByAnyOf(ctx, br, password,
		"input[name=password]",
		"input[type=password]",
	); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}

	// Step 5: submit. Press Enter so we don't need the right button name
	// across Danish/English locales; if that doesn't navigate, click a
	// "Log ind" button.
	if err := br.Press(ctx, "Enter"); err != nil {
		_ = tryClickFirst(ctx, br,
			"button:has-text('Log ind')",
			"button:has-text('Login')",
			"button[type=submit]",
		)
	}

	// Step 6: wait for redirect away from /log-ind, then confirm.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		cur, _ := br.URL(ctx)
		if cur != "" && !strings.Contains(cur, "/log-ind") {
			if ok, _ := IsLoggedIn(ctx, br); ok {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("klublotto: login did not complete within 30s")
}

// CompleteRedKontoIfVisible submits the Danske Spil Rød Konto username/password
// form only when the current page clearly is that form. It deliberately avoids
// speculative selector attempts because missing fields can block for a long
// agent-browser timeout.
func CompleteRedKontoIfVisible(ctx context.Context, br *browser.Client, username, password string) (bool, error) {
	username = stripMatchingQuotes(strings.TrimSpace(username))
	password = stripMatchingQuotes(strings.TrimSpace(password))
	if username == "" || password == "" {
		return false, errors.New("klublotto: empty username or password")
	}
	if visible, err := IsRedKontoLoginPage(ctx, br); err != nil || !visible {
		return visible, err
	}
	if err := fillByAnyOf(ctx, br, username,
		"input[name=username]",
		"input[name=login]",
		"input[autocomplete=username]",
		"input[placeholder*='Brugernavn']",
		"input[type=text]",
	); err != nil {
		return true, fmt.Errorf("fill Rød Konto username: %w", err)
	}
	if err := fillByAnyOf(ctx, br, password,
		"input[name=password]",
		"input[autocomplete=current-password]",
		"input[placeholder*='Adgangskode']",
		"input[type=password]",
	); err != nil {
		return true, fmt.Errorf("fill Rød Konto password: %w", err)
	}
	if err := br.Press(ctx, "Enter"); err != nil {
		if clickErr := tryClickFirst(ctx, br,
			"button:has-text('LOG IND')",
			"button:has-text('Log ind')",
			"button[type=submit]",
		); clickErr != nil {
			return true, fmt.Errorf("submit Rød Konto login: enter failed: %v; click failed: %w", err, clickErr)
		}
	}
	_ = br.WaitForLoad(ctx, "domcontentloaded")
	return true, nil
}

func IsRedKontoLoginPage(ctx context.Context, br *browser.Client) (bool, error) {
	cur, _ := br.URL(ctx)
	if IsRedKontoLoginURL(cur) {
		return true, nil
	}
	body, err := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
	if err != nil {
		return false, err
	}
	low := strings.ToLower(body)
	return strings.Contains(low, "log på rød konto") &&
		strings.Contains(low, "brugernavn") &&
		strings.Contains(low, "adgangskode"), nil
}

func IsRedKontoLoginURL(pageURL string) bool {
	u := strings.ToLower(pageURL)
	return strings.Contains(u, "id-dlo.danskespil.dk") &&
		strings.Contains(u, "/webflow/login")
}

func IsMitIDHandoffURL(pageURL string) bool {
	u := strings.ToLower(pageURL)
	return strings.Contains(u, "mitid.dk") ||
		strings.Contains(u, "nemlog-in")
}

func IsLoginFlowURL(pageURL string) bool {
	u := strings.ToLower(pageURL)
	return strings.Contains(u, "/log-ind") ||
		IsRedKontoLoginURL(pageURL) ||
		IsMitIDHandoffURL(pageURL)
}

func ClickLoginEntryIfVisible(ctx context.Context, br *browser.Client) (bool, error) {
	snap, err := br.SnapshotInteractive(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range snapshotLine.FindAllStringSubmatch(snap, -1) {
		role, name, ref := strings.ToLower(m[1]), strings.ToLower(strings.TrimSpace(m[2])), m[3]
		if (role == "button" || role == "link") && name == "log ind" {
			if err := br.Click(ctx, ref); err != nil {
				return true, err
			}
			return true, nil
		}
	}
	return false, nil
}

// IsLoggedIn looks for account-drawer signals that only appear after a real
// authentication. Generic "konto" icons are intentionally ignored because the
// logged-out Klub Lotto page also renders account/login chrome.
func IsLoggedIn(ctx context.Context, br *browser.Client) (bool, error) {
	var cur string
	if cur, err := br.URL(ctx); err == nil {
		if isLoggedOutURL(cur) {
			return false, nil
		}
	}

	body := ""
	if text, err := br.Eval(ctx, `document.body ? document.body.innerText : ""`); err == nil && text != "" {
		body = text
		if ok, known := loggedInState(cur, "", body); known {
			return ok, nil
		}
	}

	snap, err := br.SnapshotInteractive(ctx)
	if err != nil {
		return false, err
	}
	return looksLoggedIn(cur, snap, body), nil
}

func looksLoggedIn(pageURL, snap, body string) bool {
	ok, _ := loggedInState(pageURL, snap, body)
	return ok
}

func loggedInState(pageURL, snap, body string) (ok bool, known bool) {
	if isLoggedOutURL(pageURL) {
		return false, true
	}

	s := strings.ToLower(snap)
	if body != "" {
		s += "\n" + strings.ToLower(body)
	}

	for _, signal := range []string{
		"log ud",            // Danish "Log out"
		"min konto",         // account drawer heading
		"mine abonnementer", // account drawer menu item
		"kontohistorik",     // account drawer menu item
		"profiloplysninger", // account drawer menu item
	} {
		if strings.Contains(s, signal) {
			return true, true
		}
	}
	if accountBalanceRE.MatchString(s) &&
		(strings.Contains(s, "dine præmier") || strings.Contains(s, "spil & quiz")) {
		return true, true
	}
	if strings.Contains(s, "dagens første lod er i hus") {
		return true, true
	}
	if strings.Contains(s, "saldo") &&
		(strings.Contains(s, "indbetaling") || strings.Contains(s, "udbetaling")) {
		return true, true
	}

	// Hard logged-out signals win. The MitID and restriction pages can contain
	// phrases like "Danske Spil Rød Konto", so positive signals must be account
	// drawer/menu terms, not generic account naming.
	if strings.Contains(s, "log ind") ||
		strings.Contains(s, "opret konto") ||
		strings.Contains(s, "tilmeld") {
		return false, true
	}
	return false, false
}

func isLoggedOutURL(pageURL string) bool {
	return IsLoginFlowURL(pageURL)
}

func stripMatchingQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// tryClickFirst clicks the first selector that succeeds. All errors are
// swallowed — this helper is for best-effort dismissals.
func tryClickFirst(ctx context.Context, br *browser.Client, selectors ...string) error {
	for _, sel := range selectors {
		if err := br.Click(ctx, sel); err == nil {
			return nil
		}
	}
	return errors.New("no selector matched")
}

// fillByAnyOf tries multiple selectors in order, returning nil on the
// first successful fill.
func fillByAnyOf(ctx context.Context, br *browser.Client, value string, selectors ...string) error {
	var lastErr error
	for _, sel := range selectors {
		if err := br.Fill(ctx, sel, value); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no fill target matched")
	}
	return lastErr
}
