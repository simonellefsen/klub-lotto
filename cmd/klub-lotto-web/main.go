// Command klub-lotto-web is the Go HTTP server that ships in the
// klub-lotto pod. It does four things:
//
//  1. Renders the ledger UI (html/template + HTMX).
//  2. Exposes a small REST/HTMX API for triggering logins and game runs.
//  3. Reverse-proxies /vnc/ to the noVNC server running in the same pod
//     (port 6080), so the operator can complete MitID from the browser.
//  4. Reads/writes the Postgres source-of-truth via internal/store.
//
// Concurrency model: the long-running `klub-lotto login` / `klub-lotto quiz`
// invocations are started as goroutines; their stdout/stderr go to the
// log and to a per-job ring buffer that the UI tails. Only one runner
// can be live at a time — agent-browser's daemon is single-tenant.
package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
	"github.com/simonellefsen/klub-lotto/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

func main() {
	addr := flag.String("addr", envOr("KLUBLOTTO_WEB_ADDR", ":8080"), "HTTP listen address")
	dsn := flag.String("dsn", envOr("DATABASE_URL", ""), "Postgres DSN (or DATABASE_URL env)")
	novnc := flag.String("novnc-target", envOr("KLUBLOTTO_NOVNC_TARGET", "http://127.0.0.1:6080"), "where to proxy /vnc/")
	wikiDir := flag.String("wiki", envOr("KLUBLOTTO_WIKI_DIR", "/var/lib/klub-lotto/wiki"), "wiki directory the exporter writes to")
	binDir := flag.String("bin-dir", envOr("KLUBLOTTO_BIN_DIR", "/usr/local/bin"), "directory containing the klub-lotto CLI binary")
	flag.Parse()

	if *dsn == "" {
		log.Fatal("DATABASE_URL is required (flag --dsn or env)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	log.Println("postgres: connected and migrations applied")

	// First-boot bootstrap: if the wiki has a daily directory but the
	// DB has no ledger rows, import once. Idempotent thanks to the
	// (date, game_slug) unique key in UpsertLedger.
	if entries, err := st.ListLedger(ctx, time.Time{}, time.Time{}); err == nil && len(entries) == 0 {
		if _, statErr := os.Stat(filepath.Join(*wikiDir, "daily")); statErr == nil {
			warnings, err := st.ImportWikiDaily(ctx, *wikiDir)
			if err != nil {
				log.Printf("bootstrap import: %v", err)
			} else {
				log.Printf("bootstrap import: success (%d warnings)", len(warnings))
				for _, w := range warnings {
					log.Printf("  warn: %s", w)
				}
			}
		}
	}

	// Do not live-probe authentication on startup. The probe opens Chromium
	// against the shared agent-browser session; in Kubernetes that can steal or
	// downgrade the headed MitID browser. The UI reports the latest durable login
	// event and explicit game runs will surface real auth failures.
	if ev, err := st.LatestLoginEvent(ctx); err == nil && ev != nil {
		log.Printf("auth bootstrap: preserving latest login status=%s created_at=%s", ev.Status, ev.CreatedAt.Format(time.RFC3339))
	}

	app := &app{
		store:   st,
		novnc:   *novnc,
		wikiDir: *wikiDir,
		binDir:  *binDir,
	}
	app.templates = mustParseTemplates()

	// Best-effort: force a clean dark desktop on web startup (helps when
	// the pod restarts but the X session has old fluxbox state).
	go func() {
		time.Sleep(4 * time.Second)
		exec.Command("xsetroot", "-solid", "#1e1e2e").Run()
		exec.Command("pkill", "-f", "xmessage").Run()
		log.Println("web startup: attempted to force clean X desktop background")
	}()

	mux := http.NewServeMux()
	// "GET /{$}" matches ONLY the root path. Plain "GET /" would be a
	// subtree-rooted pattern and conflict with our methodless "/vnc/"
	// reverse-proxy registration (Go 1.22 ServeMux refuses to register
	// both — panic at boot).
	mux.HandleFunc("GET /{$}", app.handleIndex)
	mux.HandleFunc("GET /ledger", app.handleLedgerFragment)
	mux.HandleFunc("GET /ledger/{id}", app.handleLedgerDetail)
	mux.HandleFunc("GET /auth", app.handleAuthStatus)
	mux.HandleFunc("POST /actions/login", app.handleStartLogin)
	mux.HandleFunc("POST /actions/run/{game}", app.handleRunGame)
	mux.HandleFunc("GET /actions/status", app.handleJobStatus)
	mux.HandleFunc("POST /actions/debug/test-window", app.handleDebugTestWindow)
	// Direct (non-HTMX) entry points for starting jobs. These are useful when
	// the gateway TrafficPolicy only forwards the initial HTML + VNC paths but
	// drops XHR/POSTs to the action endpoints. A plain link click or curl will
	// still cause a full navigation that the gateway is more likely to forward.
	mux.HandleFunc("GET /debug/start-login", app.handleDirectStartLogin)
	mux.HandleFunc("GET /debug/start-test-window", app.handleDirectStartTestWindow)
	mux.HandleFunc("POST /actions/login/force-success", app.handleForceLoginSuccess)
	mux.Handle("/vnc/", app.novncProxy())
	mux.Handle("/vnc", app.novncProxy())
	mux.Handle("/websockify", app.novncProxy())
	mux.Handle("/websockify/", app.novncProxy())

	// The noVNC client (vnc.html + its JS) is designed to be served from the
	// root of its HTTP server. When we load it via /vnc/vnc.html, the JS still
	// requests its modules and the WebSocket at absolute paths from the origin
	// root (/core/*, /app/*, /vendor/*, and the WS path given by ?path=...).
	// We must also proxy those top-level directories to the same backend.
	rootAssets := app.novncRootAssets()
	mux.Handle("/core/", rootAssets)
	mux.Handle("/app/", rootAssets)
	mux.Handle("/vendor/", rootAssets)
	mux.Handle("/include/", rootAssets)
	mux.Handle("/sounds/", rootAssets)

	mux.Handle("/wiki/", http.StripPrefix("/wiki/", http.FileServer(http.Dir(*wikiDir))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })

	// Lightweight X11 / VNC debugging endpoint. Hit this when the noVNC view
	// is black so we can see what (if anything) is actually on display :99.
	mux.HandleFunc("GET /debug/x11", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "=== X11 / VNC debug (DISPLAY=:99) ===")
		fmt.Fprintln(w, time.Now().Format(time.RFC3339))
		fmt.Fprintln(w)

		cmds := []struct {
			name string
			args []string
		}{
			{"xsetroot (force dark bg)", []string{"xsetroot", "-solid", "#1e1e2e"}},
			{"pkill xmessage (fbsetbg dialogs)", []string{"pkill", "-f", "xmessage"}},
			{"xwininfo -root (top level windows)", []string{"xwininfo", "-root", "-tree"}},
			{"ps aux | grep -E 'Xvfb|fluxbox|x11vnc|chromium|chrome|browser|agent-browser'", []string{"sh", "-c", "ps aux | grep -E 'Xvfb|fluxbox|x11vnc|chromium|chrome|browser|agent-browser' | grep -v grep || true"}},
			{"ls -l /root/.fluxbox", []string{"ls", "-la", "/root/.fluxbox"}},
			{"Active graphical clients on :99 (xwininfo -children)", []string{"sh", "-c", "xwininfo -root -children 2>/dev/null | head -60 || true"}},
		}

		for _, c := range cmds {
			fmt.Fprintf(w, "\n--- %s ---\n", c.name)
			out, err := exec.Command(c.args[0], c.args[1:]...).CombinedOutput()
			if err != nil {
				fmt.Fprintf(w, "(exit %v)\n", err)
			}
			w.Write(out)
		}
		fmt.Fprintln(w, "\n=== end debug ===")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           withRequestLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// ---------------------------------------------------------------------------
// app — request-handler bag of dependencies
// ---------------------------------------------------------------------------

type app struct {
	store     *store.Store
	templates *template.Template
	novnc     string
	wikiDir   string
	binDir    string

	jobs jobRegistry

	// authStatusCache provides a cheap live "is the browser session valid right now?"
	// without hitting the DB history only. We cache for a short time to avoid
	// spawning the CLI on every 30s poll.
	authCacheMu   sync.Mutex
	authCache     string // "valid", "invalid", or ""
	authCacheTime time.Time
}

type webGame struct {
	Slug           string
	Name           string
	Description    string
	Runnable       bool
	DisabledReason string
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.ListLedger(r.Context(), time.Time{}, time.Time{})
	if err != nil {
		httpError(w, "list ledger", err)
		return
	}
	login, _ := a.store.LatestLoginEvent(r.Context())
	games, _ := a.store.ListGames(r.Context())
	live := a.liveAuthStatus(false)
	verified := a.liveVerified(login)

	a.render(w, "index.html", map[string]any{
		"Entries":      entries,
		"LoginEvent":   login,
		"Games":        webGames(games),
		"VNCURL":       a.vncURL(),
		"CurrentJob":   a.jobs.current(),
		"LiveStatus":   live,
		"LiveVerified": verified,
		"Forced":       false,
	})
}

func webGames(games []store.Game) []webGame {
	out := make([]webGame, 0, len(games))
	for _, g := range games {
		wg := webGame{
			Slug:        g.Slug,
			Name:        g.Name,
			Description: g.Description,
		}
		if _, ok := supportedGameSubcommands()[g.Slug]; ok {
			wg.Runnable = true
		} else {
			wg.DisabledReason = "Automation is not implemented in this build yet."
		}
		out = append(out, wg)
	}
	return out
}

func (a *app) handleLedgerFragment(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.ListLedger(r.Context(), time.Time{}, time.Time{})
	if err != nil {
		httpError(w, "list ledger", err)
		return
	}
	a.render(w, "ledger.html", map[string]any{"Entries": entries})
}

func (a *app) handleLedgerDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	e, err := a.store.GetLedgerEntry(r.Context(), id)
	if err != nil {
		httpError(w, "get entry", err)
		return
	}
	a.render(w, "detail.html", map[string]any{"Entry": e})
}

func (a *app) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	ev, _ := a.store.LatestLoginEvent(r.Context())
	force := false
	if r.URL.Query().Get("force") == "1" {
		log.Printf("auth force refresh requested; serving cached/db status to avoid starting a headless browser in the shared session")
	}
	live := a.liveAuthStatus(force)
	verified := a.liveVerified(ev)

	a.render(w, "auth.html", map[string]any{
		"LoginEvent":   ev,
		"LiveStatus":   live,
		"LiveVerified": verified,
		"Forced":       force,
	})
}

// handleStartLogin kicks off `klub-lotto login` in the background. The
// agent-browser daemon will render the MitID page on the virtual display
// (Xvfb :99 → x11vnc → noVNC), which the user sees in the embedded
// iframe. The handler returns immediately with an HTMX fragment that
// reveals the iframe and starts polling /actions/status.
func (a *app) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleStartLogin: POST received, binDir=%s, will exec 'klub-lotto login --web' with KLUBLOTTO_HEADED=true", a.binDir)
	// --web tells the login subcommand not to wait for stdin (the Enter
	// fallback), because there is no useful stdin when spawned by the web UI.
	// It will rely purely on automatic polling for the MitID completion.
	job, err := a.jobs.start(a.binDir, "login", []string{"--web"}, a.store, "login")
	if err != nil {
		log.Printf("handleStartLogin: job conflict: %v", err)
		if current := a.jobs.current(); current != nil {
			current.append("Login is already running. Use the green verify button when the browser is ready, or wait for this job to finish.")
		}
		a.render(w, "job.html", map[string]any{"Job": a.jobs.current(), "VNCURL": a.vncURL()})
		return
	}
	log.Printf("handleStartLogin: job %s started for login", job.ID)
	a.render(w, "job.html", map[string]any{"Job": job, "VNCURL": a.vncURL()})
}

// handleRunGame runs one game. The {game} path value picks the CLI subcommand.
func (a *app) handleRunGame(w http.ResponseWriter, r *http.Request) {
	game := r.PathValue("game")
	// Allowlist — never construct subcommands from user input directly.
	subcommand, ok := supportedGameSubcommands()[game]
	if !ok {
		now := time.Now()
		a.render(w, "job.html", map[string]any{
			"Job": &job{
				ID:         fmt.Sprintf("%d", now.UnixNano()),
				Action:     game,
				StartedAt:  now,
				FinishedAt: now,
				Status:     "error",
				Error:      "No automation is implemented for this game in the deployed CLI yet.",
				Log: []string{
					fmt.Sprintf("Cannot run %q: no matching klub-lotto CLI subcommand is available.", game),
					"Implemented web actions in this build: quiz.",
				},
			},
			"VNCURL": a.vncURL(),
		})
		return
	}
	job, err := a.jobs.start(a.binDir, subcommand, []string{"--submit"}, a.store, game)
	if err != nil {
		if current := a.jobs.current(); current != nil {
			current.append(fmt.Sprintf("Cannot start %s while %s is still running. Verify/finish the current job first.", game, current.Action))
		}
		a.render(w, "job.html", map[string]any{"Job": a.jobs.current(), "VNCURL": a.vncURL()})
		return
	}
	a.render(w, "job.html", map[string]any{"Job": job, "VNCURL": a.vncURL()})
}

func supportedGameSubcommands() map[string]string {
	return map[string]string{
		"quiz": "quiz",
	}
}

func (a *app) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	job := a.jobs.current()
	log.Printf("handleJobStatus: current job id=%s action=%s status=%s", func() string {
		if job != nil {
			return job.ID
		}
		return "none"
	}(), func() string {
		if job != nil {
			return job.Action
		}
		return ""
	}(), func() string {
		if job != nil {
			return job.Status
		}
		return ""
	}())
	a.render(w, "job.html", map[string]any{"Job": job, "VNCURL": a.vncURL()})
}

// handleDebugTestWindow launches a short-lived visible browser window using the
// same chromium that agent-browser drives. This is an independent "does anything
// paint on :99?" test you can trigger from the UI while the VNC tab is open.
// It bypasses the full login flow and agent-browser daemon.
func (a *app) handleDebugTestWindow(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleDebugTestWindow: spawning visible test chromium on DISPLAY=%s", os.Getenv("DISPLAY"))

	// Use a unique profile dir so we don't fight any running agent-browser session.
	profile := fmt.Sprintf("/tmp/kl-debug-%d", time.Now().UnixNano())
	url := "https://www.debian.org"

	cmd := exec.Command("chromium",
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--user-data-dir="+profile,
		"--new-window",
		"--app="+url,
	)
	cmd.Env = append(os.Environ(), "DISPLAY=:99")

	// Fire and forget; we just want the window to appear for 25s then auto-close.
	go func() {
		// Give it a life time, then kill the tree.
		time.Sleep(25 * time.Second)
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(profile)
		log.Printf("debug test-window: auto-closed after 25s")
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("debug test-window start err: %v", err)
		http.Error(w, "failed to launch test window: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return a small confirmation fragment that also reminds to look in VNC.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div style="padding:8px 12px;background:#ecfdf5;border:1px solid #10b981;border-radius:4px;">
<b>✓ Test window launched</b> — look in the VNC tab for a Debian page (it will auto-close in ~25s).<br>
<small>If nothing appears, run <code>kubectl -n klub-lotto exec deploy/klub-lotto -- xwininfo -root -tree</code> and check /debug/x11.</small>
</div>`)
}

// handleDirectStartLogin is a plain GET (no JS) that starts the MitID login job
// and redirects back to the index. Use this link when HTMX POSTs are being
// swallowed by the gateway policy.
func (a *app) handleDirectStartLogin(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleDirectStartLogin: direct GET (bypassing HTMX), binDir=%s", a.binDir)
	_, err := a.jobs.start(a.binDir, "login", []string{"--web"}, a.store, "login")
	if err != nil {
		// Job already running is not fatal for the direct path; just show current state.
		log.Printf("handleDirectStartLogin: %v (showing current state)", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDirectStartTestWindow starts the debug paint test via a plain GET + redirect.
// Same bypass purpose as above.
func (a *app) handleDirectStartTestWindow(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleDirectStartTestWindow: direct GET (bypassing HTMX)")

	profile := fmt.Sprintf("/tmp/kl-debug-%d", time.Now().UnixNano())
	url := "https://www.debian.org"

	cmd := exec.Command("chromium",
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--user-data-dir="+profile,
		"--new-window",
		"--app="+url,
	)
	cmd.Env = append(os.Environ(), "DISPLAY=:99")

	go func() {
		time.Sleep(25 * time.Second)
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(profile)
		log.Printf("direct test-window: auto-closed after 25s")
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("direct test-window start err: %v", err)
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleForceLoginSuccess verifies the visible browser before completing the
// login job. The green button means "I finished MitID", not "blindly trust the
// session"; without this check a premature click can record a false successful
// login and the next game run lands on /log-ind.
func (a *app) handleForceLoginSuccess(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleForceLoginSuccess: manual MitID completion requested by user")

	job := a.jobs.current()
	if job == nil || job.Action != "login" || job.Status != "running" {
		http.Error(w, "No running login job to mark successful", http.StatusConflict)
		return
	}

	verifyCtx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	verifiedAt, err := verifyLiveLoginSession(verifyCtx)
	if err != nil {
		msg := "MitID verification did not find a logged-in Danske Spil session yet: " + err.Error()
		log.Printf("handleForceLoginSuccess: %s", msg)
		job.append(msg)
		job.append("Leave the VNC browser open until the account page/menu is visible, then press the green button again.")
		a.render(w, "job.html", map[string]any{"Job": job, "VNCURL": a.vncURL()})
		return
	}

	job.mu.Lock()
	job.Status = "ok"
	job.FinishedAt = time.Now()
	cmd := job.cmd
	job.mu.Unlock()
	job.append("MitID login verified in live browser: " + verifiedAt)

	terminateJobProcess(cmd, "manual MitID success")

	_ = a.store.RecordLogin(r.Context(), store.LoginEvent{
		Status: "completed",
		Detail: "manual MitID completion verified in the live browser",
	})
	a.authCacheMu.Lock()
	a.authCache = "valid"
	a.authCacheTime = time.Now()
	a.authCacheMu.Unlock()

	log.Printf("handleForceLoginSuccess: login job %s verified and marked successful", job.ID)

	a.render(w, "job.html", map[string]any{"Job": job, "VNCURL": a.vncURL()})
}

func verifyLiveLoginSession(ctx context.Context) (string, error) {
	br := browser.New(envOr("AGENT_BROWSER_SESSION", envOr("AGENT_BROWSER_SESSION_NAME", "klublotto")), true)
	br.DefaultTimeout = 10 * time.Second

	var lastURL string
	var lastErr error
	submittedRedKonto := false
	var submittedRedKontoAt time.Time
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if cur, err := br.URL(ctx); err == nil && cur != "" {
			lastURL = cur
			if klublotto.IsRedKontoLoginURL(cur) {
				if submittedRedKonto {
					if time.Since(submittedRedKontoAt) > 20*time.Second {
						return "", fmt.Errorf("Rød Konto login form is still visible after one automatic submission; refusing to retry")
					}
					time.Sleep(1 * time.Second)
					continue
				}
				if _, err := klublotto.CompleteRedKontoIfVisible(ctx, br, os.Getenv("DANSKESPIL_USERNAME"), os.Getenv("DANSKESPIL_PASSWORD")); err != nil {
					return "", err
				}
				submittedRedKonto = true
				submittedRedKontoAt = time.Now()
				time.Sleep(2 * time.Second)
				continue
			}
		}
		ok, err := klublotto.IsLoggedIn(ctx, br)
		if ok {
			if lastURL == "" {
				lastURL = "current page"
			}
			return lastURL, nil
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}

	if err := br.Open(ctx, klublotto.KlubLottoURL); err != nil {
		return "", fmt.Errorf("open Klub Lotto for verification: %w", err)
	}
	_ = br.WaitForLoad(ctx, "domcontentloaded")
	cur, _ := br.URL(ctx)
	if cur != "" {
		lastURL = cur
	}
	ok, err := klublotto.IsLoggedIn(ctx, br)
	if ok {
		return lastURL, nil
	}
	if err != nil {
		lastErr = err
	}
	if lastErr != nil {
		return "", lastErr
	}
	if lastURL == "" {
		lastURL = "unknown URL"
	}
	return "", fmt.Errorf("no logged-in signal at %s", lastURL)
}

// novncProxy reverse-proxies /vnc/* to the local noVNC (websockify) server
// running inside the pod on port 6080.
//
// The previous naive implementation used NewSingleHostReverseProxy + StripPrefix,
// which does not forward WebSocket upgrade requests correctly. That is why
// you saw "Failed to connect to server" even though supervisorctl showed
// novnc and x11vnc as RUNNING.
//
// This version handles both normal HTTP (serving the noVNC client HTML/JS)
// and the WebSocket upgrade path for the actual VNC session.
func (a *app) novncProxy() http.Handler {
	target, err := url.Parse(a.novnc)
	if err != nil {
		log.Fatalf("bad --novnc-target %q: %v", a.novnc, err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origPath := r.URL.Path

		// Support being called under /vnc/ or directly at /websockify (the latter
		// happens when the noVNC client is given ?path=websockify).
		switch {
		case strings.HasPrefix(origPath, "/vnc"):
			r.URL.Path = strings.TrimPrefix(origPath, "/vnc")
		case strings.HasPrefix(origPath, "/websockify"):
			// Forward as /websockify or / to the backend. Most websockify setups
			// accept the upgrade on /websockify when the client was told that path.
			r.URL.Path = origPath // keep it; backend will see /websockify
		default:
			r.URL.Path = origPath
		}

		if r.URL.Path == "" || !strings.HasPrefix(r.URL.Path, "/") {
			r.URL.Path = "/" + strings.TrimLeft(r.URL.Path, "/")
		}

		r.URL.Host = target.Host
		r.URL.Scheme = target.Scheme
		if r.URL.Scheme == "" {
			r.URL.Scheme = "http"
		}
		r.Host = target.Host

		if isWebSocketUpgrade(r) {
			log.Printf("novnc: WS upgrade request for %s (forwarding to %s)", origPath, r.URL.Path)
			proxyWebSocket(w, r, target)
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ServeHTTP(w, r)
	})
}

// vncURL returns the URL for the embedded noVNC iframe.
// We explicitly pass path=websockify because we are serving noVNC under
// the /vnc/ subpath through our reverse proxy. Without it, the JavaScript
// client often computes the wrong WebSocket endpoint and fails with
// "Failed to connect to server".
func (a *app) vncURL() string {
	// Base noVNC URL (relative). The client-side JS in index.html will
	// dynamically set the correct `path` parameter for websockify based
	// on the current public URL prefix (important for gateway subpaths
	// like /klub-lotto).
	return "vnc/vnc.html?autoconnect=1&resize=scale&reconnect=1"
}

// liveAuthStatus returns "valid", "invalid", or "unknown" by asking the
// actual browser session (via the CLI with --check). Results are cached
// briefly so the 30s HTMX poll doesn't spawn processes constantly.
// Supports force=true to bypass cache and re-probe immediately.
// Robust: short timeout, better VALID/INVALID parse, fallback to latest
// login_event on transient probe failure. Expiry hint is "re-verified on demand".
func (a *app) liveAuthStatus(force bool) string {
	const cacheTTL = 75 * time.Second

	if job := a.jobs.current(); job != nil && job.Status == "running" {
		a.authCacheMu.Lock()
		cached := a.authCache
		cacheFresh := time.Since(a.authCacheTime) < cacheTTL
		a.authCacheMu.Unlock()
		if cached != "" && cacheFresh {
			return cached
		}
		return "unknown"
	}

	// Fast path + force clear under lock only (short critical section).
	a.authCacheMu.Lock()
	if force {
		a.authCache = ""
		a.authCacheTime = time.Time{}
	}
	if !force && time.Since(a.authCacheTime) < cacheTTL && a.authCache != "" {
		cached := a.authCache
		a.authCacheMu.Unlock()
		return cached
	}
	a.authCacheMu.Unlock()

	if !force {
		return "unknown"
	}

	// Probe outside the lock: prevents 15s head-of-line blocking on /auth
	// (30s pill poll + manual refresh).
	probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, filepath.Join(a.binDir, "klub-lotto"), "login", "--check", "--web")
	cmd.Env = append(os.Environ(),
		"KLUBLOTTO_HEADED=true",
		"AGENT_BROWSER_HEADED=true",
		"AGENT_BROWSER_SESSION="+envOr("AGENT_BROWSER_SESSION", envOr("AGENT_BROWSER_SESSION_NAME", "klublotto")),
		"AGENT_BROWSER_SESSION_NAME="+envOr("AGENT_BROWSER_SESSION_NAME", envOr("AGENT_BROWSER_SESSION", "klublotto")),
		"AGENT_BROWSER_PROFILE="+envOr("AGENT_BROWSER_PROFILE", "/var/lib/agent-browser/chrome-profile"),
	)

	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))

	status := "unknown"
	didProbe := true // live CLI attempt (success or fail/timeout); used for accurate "last verified" timestamp
	if err == nil && strings.Contains(outStr, "VALID") {
		status = "valid"
	} else if strings.Contains(outStr, "INVALID") {
		status = "invalid"
	} else if err != nil || probeCtx.Err() == context.DeadlineExceeded {
		didProbe = false // do not claim "just now" fresh verification on inconclusive probes
	}

	// Short write lock only for cache update. Timestamp only on actual probe (addresses inaccurate "last verified" after fallback).
	a.authCacheMu.Lock()
	a.authCache = status
	if didProbe {
		a.authCacheTime = time.Now()
	}
	a.authCacheMu.Unlock()
	return status
}

// liveVerified returns a human string for the last safe verification time.
func (a *app) liveVerified(ev *store.LoginEvent) string {
	a.authCacheMu.Lock()
	cacheTime := a.authCacheTime
	a.authCacheMu.Unlock()
	if !cacheTime.IsZero() {
		d := time.Since(cacheTime)
		if d < 10*time.Second {
			return "just now"
		}
		return fmt.Sprintf("%ds ago", int(d.Round(time.Second).Seconds()))
	}
	if ev != nil && (ev.Status == "completed" || ev.Status == "session-reused") {
		return ev.CreatedAt.Format("2006-01-02 15:04 UTC")
	}
	return "never"
}

// novncRootAssets returns a reverse proxy for the top-level directories
// that the noVNC JavaScript (loaded via /vnc/vnc.html) requests at the
// origin root. Without these, the client gets 404s for its modules and
// never successfully opens the WebSocket.
func (a *app) novncRootAssets() http.Handler {
	target, err := url.Parse(a.novnc)
	if err != nil {
		log.Fatalf("bad --novnc-target %q: %v", a.novnc, err)
	}
	return httputil.NewSingleHostReverseProxy(target)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func proxyWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL) {
	// Dial the backend (x11vnc via websockify).
	backend, err := net.Dial("tcp", target.Host)
	if err != nil {
		http.Error(w, "cannot reach VNC backend: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()

	// Forward the client's upgrade request to the backend.
	r.URL.Scheme = target.Scheme
	r.URL.Host = target.Host
	if err := r.Write(backend); err != nil {
		http.Error(w, "forwarding upgrade request failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Read the backend's 101 Switching Protocols response.
	br := bufio.NewReader(backend)
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		http.Error(w, "reading backend upgrade response: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack the downstream (browser) connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "server does not support connection hijacking", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	// Send the 101 response (headers + any body) back to the browser.
	if err := resp.Write(client); err != nil {
		return
	}

	// Now pump raw bytes in both directions. This carries the WebSocket frames.
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, client); done <- struct{}{} }()
	go func() { io.Copy(client, backend); done <- struct{}{} }()
	<-done
}

func (a *app) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// jobs — single-slot registry of background CLI runs
// ---------------------------------------------------------------------------

type jobRegistry struct {
	mu  sync.Mutex
	cur *job
}

type job struct {
	ID         string
	Action     string // login | quiz | ordknude
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string // running | ok | error
	Error      string
	Log        []string
	cmd        *exec.Cmd
	mu         sync.Mutex
}

func (j *job) append(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Log = append(j.Log, line)
	if len(j.Log) > 500 {
		j.Log = j.Log[len(j.Log)-500:]
	}
}

func (j *job) hasLogText(needle string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	needle = strings.ToLower(needle)
	for _, line := range j.Log {
		if strings.Contains(strings.ToLower(line), needle) {
			return true
		}
	}
	return false
}

func (r *jobRegistry) current() *job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

func (r *jobRegistry) start(binDir, sub string, args []string, st *store.Store, action string) (*job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.cur.Status == "running" {
		return nil, fmt.Errorf("another job is already running: %s", r.cur.Action)
	}
	bin := filepath.Join(binDir, "klub-lotto")
	cmd := exec.Command(bin, append([]string{sub}, args...)...)
	cmd.Env = append(os.Environ(),
		"KLUBLOTTO_HEADED=true",     // our CLI should request headed mode
		"AGENT_BROWSER_HEADED=true", // agent-browser should launch a visible Chromium window
		"AGENT_BROWSER_ARGS=--start-maximized --disable-session-crashed-bubble",
		"AGENT_BROWSER_SESSION="+envOr("AGENT_BROWSER_SESSION", envOr("AGENT_BROWSER_SESSION_NAME", "klublotto")),
		"AGENT_BROWSER_SESSION_NAME="+envOr("AGENT_BROWSER_SESSION_NAME", envOr("AGENT_BROWSER_SESSION", "klublotto")),
		"AGENT_BROWSER_PROFILE="+envOr("AGENT_BROWSER_PROFILE", "/var/lib/agent-browser/chrome-profile"),
	)

	j := &job{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Action:    action,
		StartedAt: time.Now(),
		Status:    "running",
		cmd:       cmd,
	}
	r.cur = j

	// Extra diagnostics so the user can see exactly what is being launched
	// for the MitID login (very useful while the VNC is black).
	fullArgv := append([]string{bin, sub}, args...)
	log.Printf("starting job %s: argv=%v", j.ID, fullArgv)
	log.Printf("job %s cwd=%s", j.ID, func() string {
		if cmd.Dir != "" {
			return cmd.Dir
		}
		d, _ := os.Getwd()
		return d
	}())
	log.Printf("job %s env KLUBLOTTO_HEADED=%s AGENT_BROWSER_HEADED=%s AGENT_BROWSER_ARGS=%q AGENT_BROWSER_PROFILE=%s DISPLAY=%s HOME=%s AGENT_BROWSER_SESSION=%s AGENT_BROWSER_SESSION_NAME=%s",
		j.ID,
		envValue(cmd.Env, "KLUBLOTTO_HEADED"),
		envValue(cmd.Env, "AGENT_BROWSER_HEADED"),
		envValue(cmd.Env, "AGENT_BROWSER_ARGS"),
		envValue(cmd.Env, "AGENT_BROWSER_PROFILE"),
		envValue(cmd.Env, "DISPLAY"),
		envValue(cmd.Env, "HOME"),
		envValue(cmd.Env, "AGENT_BROWSER_SESSION"),
		envValue(cmd.Env, "AGENT_BROWSER_SESSION_NAME"))

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		j.Status = "error"
		j.Error = err.Error()
		j.FinishedAt = time.Now()
		return j, nil
	}
	go pipeToJob(stdout, j)
	go pipeToJob(stderr, j)
	go func() {
		err := cmd.Wait()
		j.mu.Lock()
		if j.FinishedAt.IsZero() {
			j.FinishedAt = time.Now()
		}
		if j.Status == "running" {
			if err != nil {
				j.Status = "error"
				j.Error = err.Error()
			} else {
				j.Status = "ok"
			}
		}
		status := j.Status
		action := j.Action
		j.mu.Unlock()
		log.Printf("job %s (%s) finished: status=%s", j.ID, action, status)
		// Always record the outcome of login attempts. This ensures Postgres
		// has an up-to-date view of authentication state even if the job
		// ended in error for transient reasons (the underlying browser
		// session may still be valid).
		if action == "login" {
			loginStatus := "failed"
			if status == "ok" {
				loginStatus = "completed"
			}
			_ = st.RecordLogin(context.Background(), store.LoginEvent{
				Status: loginStatus,
				Detail: "via web UI job",
			})
		}
		if action != "login" && status == "error" && j.hasLogText("login required") {
			_ = st.RecordLogin(context.Background(), store.LoginEvent{
				Status: "failed",
				Detail: fmt.Sprintf("%s run landed on the Danske Spil login page; fresh verified MitID login required", action),
			})
		}
		// Re-import the wiki after game runs so any new daily/sources/
		// content lands in the DB. The CLI doesn't yet write to the DB
		// directly — that's an open item tracked in
		// wiki/concepts/k8s-deploy.md.
		if action != "login" && status == "ok" {
			wikiDir := os.Getenv("KLUBLOTTO_WIKI_DIR")
			if wikiDir == "" {
				wikiDir = "/var/lib/klub-lotto/wiki"
			}
			if _, err := st.ImportWikiDaily(context.Background(), wikiDir); err != nil {
				log.Printf("post-run wiki import: %v", err)
			}
		}
	}()
	return j, nil
}

func pipeToJob(rd io.Reader, j *job) {
	buf := make([]byte, 1024)
	carry := ""
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			carry += string(buf[:n])
			for {
				idx := indexOf(carry, "\n")
				if idx < 0 {
					break
				}
				line := carry[:idx]
				carry = carry[idx+1:]
				j.append(line)
				log.Printf("[%s] %s", j.Action, line)
			}
		}
		if err != nil {
			if carry != "" {
				j.append(carry)
			}
			return
		}
	}
}

func indexOf(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func mustParseTemplates() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{
		"date": func(t time.Time) string { return t.Format("2006-01-02") },
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Format("2006-01-02 15:04 MST")
		},
		"yesno": func(b bool) string {
			if b {
				return "✓"
			}
			return "—"
		},
		"base64": func(b []byte) string {
			return base64.StdEncoding.EncodeToString(b)
		},
		// dict lets index.html inline a sub-template with a custom
		// data map (Go html/template doesn't have map literals).
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict requires even arg count")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict key %d not string", i)
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
	}).ParseFS(tmplFS, "templates/*.html"))
}

func withRequestLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		// Log the raw path the pod actually received (after any gateway rewrite).
		// Also surface X-Forwarded-* so we can see what the ngrok gateway sent us.
		fwd := r.Header.Get("X-Forwarded-Host")
		if fwd != "" {
			log.Printf("%s %s %s (fwd=%s xfproto=%s xfpath=%s)",
				r.Method, r.URL.Path, time.Since(start),
				fwd,
				r.Header.Get("X-Forwarded-Proto"),
				r.Header.Get("X-Forwarded-Path"))
		} else {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
		}
	})
}

func httpError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	http.Error(w, what+": "+err.Error(), http.StatusInternalServerError)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func closeAgentBrowserSession(parent context.Context) {
	session := envOr("AGENT_BROWSER_SESSION", envOr("AGENT_BROWSER_SESSION_NAME", "klublotto"))
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "agent-browser", "--json", "--session", session, "--session-name", session, "close")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("agent-browser close session %q: %v (%s)", session, err, strings.TrimSpace(string(out)))
	}
}

func terminateJobProcess(cmd *exec.Cmd, reason string) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	log.Printf("terminating job process pid=%d: %s", cmd.Process.Pid, reason)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("terminate job process pid=%d: %v", cmd.Process.Pid, err)
		return
	}
	go func() {
		time.Sleep(2 * time.Second)
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
	}()
}
