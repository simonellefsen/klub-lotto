import subprocess, time, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from blok import load, read_board, read_pieces

# --- agent-browser plumbing -------------------------------------------------
BIN = os.environ.get("AGENT_BROWSER_BIN",
                     "/Users/lindau/codex/agent-browser/cli/target/release/agent-browser")
ENV = dict(os.environ, AGENT_BROWSER_SESSION="klublotto",
           AGENT_BROWSER_SESSION_NAME="klublotto", _ZO_DOCTOR="0")
SHOTDIR = os.environ.get("BLOK_SHOTDIR", "/tmp")

def ab(*a):
    return subprocess.run([BIN, *a], env=ENV, capture_output=True, text=True).stdout

def resize_fix():
    # The cross-origin game canvas collapses to 30px on (re)embed; an in-frame
    # resize event recovers it to the real 426px. Harmless when not collapsed.
    ab("frame", "iframe.kl-game__iframe")
    ab("eval", '(function(){window.dispatchEvent(new Event("resize"));return 1;})()')
    ab("frame", "main")

def shot(p):
    ab("screenshot", p); return p

NEUTRAL = (40, 380)  # off-board, in-viewport spot to release a stuck pickup

def mouse_release_safe():
    # Drop anything that might be held, away from the board, so a botched drag
    # can't leave a piece stuck to the cursor.
    ab("mouse", "move", str(NEUTRAL[0]), str(NEUTRAL[1])); time.sleep(0.1)
    ab("mouse", "up"); time.sleep(0.3)

# --- perception -------------------------------------------------------------
def _key(g, pieces):
    return (tuple(map(tuple, g)),
            tuple((p['h'], p['w'], tuple(map(tuple, p['shape']))) for p in pieces))

def read_once(tag="rd"):
    im = load(shot(os.path.join(SHOTDIR, tag + ".png")))
    g, geom, _ = read_board(im)
    pieces = read_pieces(im, geom)
    return g, geom, pieces

def read_settled(tries=6):
    """Return (board, geom, pieces) once two consecutive reads agree. The drag
    animation forces a fresh OOPIF composite, so post-move reads are fresh; this
    mostly guards against catching a mid-animation frame. Retries through blank/
    collapsed-canvas frames via resize_fix."""
    prev = None; last = None
    for _ in range(tries):
        resize_fix(); time.sleep(0.35)
        try:
            g, geom, pieces = read_once()
        except Exception:
            time.sleep(0.4); continue
        last = (g, geom, pieces)
        key = _key(g, pieces)
        if key == prev and pieces:
            return g, geom, pieces
        prev = key; time.sleep(0.3)
    if last is None:
        raise RuntimeError("could not read board (canvas blank/collapsed?)")
    return last

def read_scores():
    """Read the live (current, best) score straight from the game's DOM inside the
    iframe — `.game-current-score-value` / `.game-best-score-value`. Unlike the
    WebGL board, these are plain DOM text nodes, so the read is exact and immune to
    the stale-canvas problem. Returns (current, best) as ints, or None for any value
    that couldn't be read (e.g. on the win/intro screen). Leaves the frame on main."""
    ab("frame", "iframe.kl-game__iframe")
    out = ab("eval",
             '(function(){function n(s){var e=document.querySelector(s);'
             'if(!e)return "";return (e.textContent||"").replace(/[^0-9]/g,"");}'
             'return n(".game-current-score-value")+"|"+n(".game-best-score-value");})()')
    ab("frame", "main")
    out = out.strip().strip('"')
    parts = out.split("|")
    def toi(s):
        s = s.strip()
        return int(s) if s.isdigit() else None
    if len(parts) == 2:
        return toi(parts[0]), toi(parts[1])
    return None, None

# --- mechanics --------------------------------------------------------------
def drag(p, q, steps=30):
    """Smooth, stepped pointer drag. Phaser only lifts the piece for continuous
    small steps; big jumps are rejected. Pick up, hold, glide, release."""
    ab("mouse", "move", str(int(p[0])), str(int(p[1]))); time.sleep(0.2)
    ab("mouse", "down"); time.sleep(0.32)
    for i in range(1, steps + 1):
        x = p[0] + (q[0] - p[0]) * i / steps
        y = p[1] + (q[1] - p[1]) * i / steps
        ab("mouse", "move", str(int(x)), str(int(y))); time.sleep(0.035)
    time.sleep(0.18); ab("mouse", "up"); time.sleep(0.7)

def release_vp(geom, shape, r, c):
    """Viewport pixel to release at so the piece's top-left lands on board cell
    (r,c): the footprint CENTRE. Clamped to stay inside the 8x8 board."""
    x0, y0, cell = geom; bxv, byv, cv = x0 / 2, y0 / 2, cell / 2
    h, w = len(shape), len(shape[0])
    rx = bxv + (c + w / 2) * cv
    ry = byv + (r + h / 2) * cv
    rx = min(max(rx, bxv + cv * 0.35), bxv + 8 * cv - cv * 0.35)
    ry = min(max(ry, byv + cv * 0.35), byv + 8 * cv - cv * 0.35)
    return rx, ry

# --- solver -----------------------------------------------------------------
def valid(b, s):
    h = len(s); w = len(s[0]); o = []
    for r in range(9 - h):
        for c in range(9 - w):
            if all(not (s[i][j] and b[r + i][c + j])
                   for i in range(h) for j in range(w)):
                o.append((r, c))
    return o

def apply_p(b, s, r, c):
    """Place shape s at (r,c) and clear full rows/cols. Returns (board, n_cleared)."""
    g = [row[:] for row in b]
    for i in range(len(s)):
        for j in range(len(s[0])):
            if s[i][j]:
                g[r + i][c + j] = 1
    fr = [i for i in range(8) if all(g[i])]
    fc = [j for j in range(8) if all(g[i][j] for i in range(8))]
    for i in fr:
        for j in range(8): g[i][j] = 0
    for j in fc:
        for i in range(8): g[i][j] = 0
    return g, len(fr) + len(fc)

def quality(b):
    """Reward open space; HEAVILY penalize dead 1x1 holes (no piece is 1x1) and
    tight single-neighbour gaps that are hard to fill."""
    empty = dead = tight = 0
    for r in range(8):
        for c in range(8):
            if not b[r][c]:
                empty += 1
                en = sum(1 for dr, dc in ((1, 0), (-1, 0), (0, 1), (0, -1))
                         if 0 <= r + dr < 8 and 0 <= c + dc < 8 and not b[r + dr][c + dc])
                if en == 0: dead += 1
                elif en == 1: tight += 1
    return empty - 45 * dead - 4 * tight

def plan(board, shapes):
    """Full-trio lookahead. Returns ranked first-moves [(score,(pi,r,c)),...],
    best first. Score = 120*lines_cleared + board quality at the end of the trio."""
    results = {}
    def rec(bd, rem, first, cl):
        if first is not None:
            sc = cl * 120 + quality(bd)
            if first not in results or sc > results[first][0]:
                results[first] = (sc,)
        for k in range(len(rem)):
            pi = rem[k]; sh = shapes[pi]
            for (r, c) in valid(bd, sh):
                nb, n = apply_p(bd, sh, r, c)
                rec(nb, rem[:k] + rem[k + 1:],
                    first if first is not None else (pi, r, c), cl + n)
    rec(board, list(range(len(shapes))), None, 0)
    return sorted(((v[0], mv) for mv, v in results.items()), reverse=True)

# --- driver -----------------------------------------------------------------
def cells(b): return sum(sum(r) for r in b)

def place_and_verify(board, geom, piece, r, c):
    """Drag the piece and confirm it landed. Compares the new read against the
    PREDICTED board (so a placement that immediately clears a line — which can
    leave the board identical or emptier — still reads as success, where the old
    `board2==board` test wrongly called it a failure). Returns (status, board2)
    with status in {'ok','fail'}."""
    shape = piece['shape']
    predicted, _ = apply_p(board, shape, r, c)
    rx, ry = release_vp(geom, shape, r, c)
    drag(piece['pick_vp'], (rx, ry))
    board2, _, _ = read_settled()
    if board2 == predicted:
        return 'ok', board2
    if board2 == board:
        mouse_release_safe()              # nothing changed → make sure nothing's held
        return 'fail', board2
    # Unexpected board: the drag did *something*, or perception drifted. Treat as a
    # soft-success and let the next receding-horizon re-read re-plan from reality.
    return 'ok', board2

def main(target=100000, max_steps=2000, goal_score=int(os.environ.get("BLOK_GOAL", "0"))):
    # By default (goal_score<=0, target huge) the run plays on until the board can't
    # take any more pieces — game-over — to maximise the score. Set BLOK_GOAL>0 to
    # stop early once the live score reaches it (e.g. just earn the 200-pt lod).
    placed_cells = 0; steps = 0; stuck = 0
    bad = set()                              # (piece-signature, r, c) moves to avoid this trio
    last_sig = None
    # Per-move score record (current + best, read from the DOM). One row per placed
    # piece; also echoed to stdout. Lives next to the screenshots.
    rec_path = os.path.join(SHOTDIR, "blok-scores.csv")
    rec = open(rec_path, "w", buffering=1)
    rec.write("step,piece,row,col,current_score,best_score,placed_cells,board_filled\n")
    print("recording per-move scores to %s (%s)" % (
        rec_path, ("stop at current_score>=%d" % goal_score) if goal_score > 0
        else "playing to game-over for max score"))
    while placed_cells < target and steps < max_steps:
        steps += 1
        try:
            board, geom, pieces = read_settled()
        except RuntimeError as e:
            # The board can't be read because it's gone — at the end of a healthy
            # run this is the WIN / game-over screen replacing the board, not a
            # fault. Capture it and stop cleanly rather than crashing.
            shot(os.path.join(SHOTDIR, "blok_final.png"))
            print("[%d] board gone (%s) — likely game complete (win/game-over). "
                  "cells_placed~%d. See blok_final.png" % (steps, e, placed_cells))
            rec.close(); print("score record written to %s" % rec_path)
            return placed_cells
        sig = _key(board, pieces)
        if sig != last_sig:                  # board/tray changed → fresh trio context
            bad = set(); last_sig = sig
        if not pieces:
            print("[%d] tray empty; waiting for refill" % steps); time.sleep(1.2); continue
        shapes = [p['shape'] for p in pieces]
        ranked = plan(board, shapes)
        ranked = [(s, mv) for (s, mv) in ranked
                  if (tuple(map(tuple, shapes[mv[0]])), mv[1], mv[2]) not in bad]
        if not ranked:
            stuck += 1
            print("[%d] no non-failed move available (stuck=%d)" % (steps, stuck))
            if stuck >= 3:
                print("GAME OVER / unsolvable from here"); break
            time.sleep(0.8); continue
        _, (pi, r, c) = ranked[0]; piece = pieces[pi]
        h, w = piece['h'], piece['w']
        status, board2 = place_and_verify(board, geom, piece, r, c)
        if status == 'fail':
            bad.add((tuple(map(tuple, shapes[pi])), r, c))
            print("[%d] FAILED %dx%d@(%d,%d) — will try next-best" % (steps, h, w, r, c))
            continue
        stuck = 0
        placed_cells += sum(sum(row) for row in piece['shape'])
        last_sig = _key(board2, [])          # force fresh trio detection next loop
        cur, best = read_scores()            # exact score straight from the DOM
        rec.write("%d,%dx%d,%d,%d,%s,%s,%d,%d\n"
                  % (steps, h, w, r, c,
                     "" if cur is None else cur, "" if best is None else best,
                     placed_cells, cells(board2)))
        print("[%d] placed %dx%d@(%d,%d)  score=%s  best=%s  cells_placed~%d  board_filled=%d"
              % (steps, h, w, r, c,
                 "?" if cur is None else cur, "?" if best is None else best,
                 placed_cells, cells(board2)))
        if goal_score > 0 and cur is not None and cur >= goal_score:
            print("[%d] GOAL REACHED: current score %d >= %d" % (steps, cur, goal_score))
            break
    rec.close()
    print("score record written to %s" % rec_path)
    shot(os.path.join(SHOTDIR, "blok_final.png"))
    print("DONE: ~%d points placed in %d steps (target %d)" % (placed_cells, steps, target))
    return placed_cells

if __name__ == "__main__":
    tgt = int(sys.argv[1]) if len(sys.argv) > 1 else 100000
    main(tgt)
