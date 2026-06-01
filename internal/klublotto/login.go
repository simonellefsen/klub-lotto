package klublotto

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

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
	if err := br.Open(ctx, KlubLottoURL); err != nil {
		return fmt.Errorf("open klublotto: %w", err)
	}
	_ = br.WaitForLoad(ctx, "networkidle")

	if loggedIn, _ := IsLoggedIn(ctx, br); loggedIn {
		return nil
	}

	// Step 2: navigate to the login form.
	if err := br.Open(ctx, LoginURL); err != nil {
		return fmt.Errorf("open login: %w", err)
	}
	_ = br.WaitForLoad(ctx, "networkidle")

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

// IsLoggedIn looks for account-drawer signals that only appear after a real
// authentication. Generic "konto" icons are intentionally ignored because the
// logged-out Klub Lotto page also renders account/login chrome.
func IsLoggedIn(ctx context.Context, br *browser.Client) (bool, error) {
	snap, err := br.SnapshotInteractive(ctx)
	if err != nil {
		return false, err
	}
	s := strings.ToLower(snap)
	for _, signal := range []string{
		"log ud",            // Danish "Log out"
		"min konto",         // "My account"
		"rød konto",         // account drawer for logged-in users
		"saldo",             // balance panel in the account drawer
		"indbetaling",       // deposit button in the account drawer
		"udbetaling",        // withdrawal button in the account drawer
		"mine abonnementer", // account drawer menu item
		"kontohistorik",     // account drawer menu item
		"profiloplysninger", // account drawer menu item
	} {
		if strings.Contains(s, signal) {
			return true, nil
		}
	}
	// Negative signal — if a prominent "Log ind" CTA exists, we are not logged in.
	if strings.Contains(s, "log ind") {
		return false, nil
	}
	return false, nil
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
