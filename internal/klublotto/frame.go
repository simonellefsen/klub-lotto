package klublotto

import (
	"context"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// Frame helpers for the embedded game iframe (the cross-origin immerspiele
// OOPIF, selector GameIframe). agent-browser can eval/click inside the OOPIF
// once the context is switched into it; callers pair EnterGameFrame with a
// deferred LeaveFrame to return to the top document.

// EnterGameFrame switches the browser context into the game iframe. It returns
// the switch error so callers can fall back (e.g. to a frames-inclusive parent
// snapshot) when the frame isn't present.
func EnterGameFrame(ctx context.Context, br *browser.Client) error {
	return br.Frame(ctx, GameIframe)
}

// LeaveFrame returns the browser context to the top document. It uses a
// background context so it still runs during deferred cleanup even if the
// caller's ctx was already cancelled.
func LeaveFrame(br *browser.Client) {
	_ = br.Frame(context.Background(), "")
}
