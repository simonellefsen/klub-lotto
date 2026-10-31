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
//
// The target is "main". This used to pass "", which agent-browser rejects
// ("DOM Error while querying") — and the error was discarded, so LeaveFrame was
// silently a no-op and every caller stayed inside the game iframe. Code that
// then measured the PARENT page (the Ordkløver vision crop) measured the frame
// instead and cropped the board out of the picture, and a run that died in a
// frame left the next one pointed at a frame that no longer existed.
func LeaveFrame(br *browser.Client) {
	_ = br.Frame(context.Background(), "main")
}
