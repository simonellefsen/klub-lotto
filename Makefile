# klub-lotto — convenience targets.
#
# All commands assume the user has Go, agent-browser, and (optionally) qmd
# on PATH. Run `make doctor` to verify.

BIN := bin/klub-lotto

AGENT_BROWSER_SESSION ?= klublotto
AGENT_BROWSER_SESSION_NAME ?= klublotto
AGENT_BROWSER_HEADED ?= true
# Point at the local Rust build of agent-browser that can see embedded iframe elements.
# Override with: make sudoku AGENT_BROWSER_BIN=agent-browser (to use PATH version instead)
AGENT_BROWSER_BIN ?= /Users/lindau/codex/agent-browser/cli/target/release/agent-browser
LOCAL_BROWSER_ENV := AGENT_BROWSER_SESSION=$(AGENT_BROWSER_SESSION) AGENT_BROWSER_SESSION_NAME=$(AGENT_BROWSER_SESSION_NAME) AGENT_BROWSER_HEADED=$(AGENT_BROWSER_HEADED) AGENT_BROWSER_BIN=$(AGENT_BROWSER_BIN)
# Vision model for reading the Ordkløver/Krydsord board. A "~"-prefixed slug is
# a valid OpenRouter floating alias (resolves to the current concrete model).
# Override the vision model with either VISION_MODEL=... or
# OPENROUTER_VISION_MODEL=... e.g. `make krydsord VISION_MODEL=~google/gemini-pro-latest`.
OPENROUTER_VISION_MODEL ?= $(or $(VISION_MODEL),openai/gpt-5.5)
GAME_ANSWER := $(or $(ANSWER),$(answer),$(SOLUTION),$(solution))
GAME_PROVIDER := $(or $(PROVIDER),$(provider))
GAME_PROVIDER_FLAG := $(if $(GAME_PROVIDER),--provider "$(GAME_PROVIDER)")
GAME_FINAL_PROVIDER := $(or $(FINAL_PROVIDER),$(final_provider))
GAME_FINAL_PROVIDER_FLAG := $(if $(GAME_FINAL_PROVIDER),--final-provider "$(GAME_FINAL_PROVIDER)")
GAME_AUTO_ANSWER_FLAG := $(if $(filter true 1 yes,$(AUTO_ANSWER)),--auto-answer)

# Ordkløver attempt-tiered models. The loop uses the FAST model while attempts
# < 7/12, then switches in-code to the REASON model at >= 7/12 (and for the
# final guess). Override with PROVIDER=/WORD_PROVIDER= (fast) or FINAL_PROVIDER=
# (reason). "~author/model-latest" slugs are OpenRouter floating aliases.
# "openai:<model>" (colon, no slash) routes to the native OpenAI subscription
# (OPENAI_API_KEY) — NOT OpenRouter's "openai/<model>" catalog slug.
ORDKLOEVER_FAST   := $(or $(PROVIDER),$(provider),$(WORD_PROVIDER),openai:gpt-5.6-terra)
ORDKLOEVER_REASON := $(or $(FINAL_PROVIDER),$(final_provider),$(ORDKLOEVER_FINAL_PROVIDER),~google/gemini-pro-latest)

# Ordknuden default word model. Override with PROVIDER=/WORD_PROVIDER=/ORDKNUDE_PROVIDER=.
# "openai:<model>" routes to the native OpenAI subscription, not OpenRouter.
ORDKNUDE_MODEL := $(or $(PROVIDER),$(provider),$(WORD_PROVIDER),$(ORDKNUDE_PROVIDER),openai:gpt-5.6-luna)
ORDKNUDE_PROVIDER_FLAG := --provider "$(ORDKNUDE_MODEL)"

.PHONY: help build vet test check doctor login quiz quiz-dry sudoku sudoku-dry ordkloever ordkloever-dry ordkloever-extract ordkloever-probe ordknude ordknude-dry ordknude-extract krydsord krydsord-dry krydsord-graph krydsord-solve krydsord-solve-dry blok wiki-query wiki-lint sync clean reset \
        image deploy k8s-up k8s-down k8s-logs port-forward db-shell ui-url tidy \
        db-up db-down db-import db-port-forward

help:
	@echo "make build       — build the CLI into ./bin/klub-lotto"
	@echo "make test        — run all Go unit tests (offline; no browser)"
	@echo "make check       — build + go vet + go test (run before committing)"
	@echo "make doctor      — sanity-check config, providers, agent-browser"
	@echo "make login       — interactive login (headed; saves session)"
	@echo "make quiz        — solve today's Quiz (headed), commit wiki, push"
	@echo "make quiz-dry    — same but doesn't click or sync; just shows what we would do"
	@echo "make sudoku-dry  — extract and solve today's Sudoku locally; no submit"
	@echo "make sudoku      — submit today's Sudoku through the parent page"
	@echo "make ordkloever-dry — extract state and print Danish candidates; no guessing"
	@echo "make ordkloever-extract — extract Ordkløver state only; no provider, no guessing"
	@echo "make ordkloever  — auto-play (probe letters + real submit full phrase via parent+frame kb until solved): bare or make ordkloever ANSWER=... (permanent, no do-overs)"
	@echo "make ordkloever-probe — submit letter probes, no final answer unless AUTO_ANSWER=true"
	@echo "make ordknude-dry — extract state and print Danish candidates; no guessing"
	@echo "make ordknude-extract — extract Ordknuden state only; no provider, no guessing"
	@echo "make ordknude    — auto-play (LLM proposes + real submits guesses until solved): make ordknude ANSWER=SALÆR (or bare for full auto from blank; permanent, no do-overs)"
	@echo "make krydsord-dry — extract today's Krydsord board image, mask, and slots; no submit"
	@echo "make krydsord      — real solve (vision clues + candidates + grid) + submit via API + Tjek løsning on parent (reuses klublotto session)"
	@echo "make krydsord-graph— stage 1: deconstruct + verify the clue graph (JSON); GRAPH_MODEL= to try a vision LLM, VERIFY=false to skip the check; no solve/submit"
	@echo "make krydsord-solve— stage 2/3: solve from a validated graph (GRAPH_FILE=, SOLVE_MODEL=); SUBMIT=true fills + submits the board"
	@echo "make krydsord-solve-dry — build the board + CSP prompt only (no LLM call), for inspecting/iterating"
	@echo "make blok        — play today's Blok for Blok to game-over, maximising score (native Go: pixel perception + lookahead solver). GOAL=<n> to stop early at a score (e.g. GOAL=200 to just earn the lod)"
	@echo "make wiki-query Q='...'  — search the wiki via qmd (or grep)"
	@echo "make sync        — commit wiki/doc changes and push to origin"
	@echo "make park        — unload the danskespil tab (→ about:blank) when done for the day, keeping the session; stops the heavy page wedging Chrome overnight"
	@echo "make reset       — close any agent-browser daemons (run if you can't see the window)"
	@echo ""
	@echo "k8s targets (docker-desktop)"
	@echo "make image       — docker build, tagged with git short SHA + :dev"
	@echo "make k8s-up      — first-time apply of all manifests in deploy/k8s/"
	@echo "make deploy      — rebuild image and roll the deployment to the new sha tag"
	@echo "make k8s-down    — delete the namespace (drops data!)"
	@echo "make k8s-logs    — tail the app pod"
	@echo "make port-forward— kubectl port-forward to http://localhost:8080"
	@echo "make ui-url      — print the ingress URL (http://klub-lotto.localhost)"
	@echo "make db-shell    — psql into the cnpg primary"
	@echo "make db-up       — bring up a local backup-less cnpg klublotto DB"
	@echo "make db-import   — sync wiki/daily/*.md into Postgres (DB = source of truth)"
	@echo "make db-down     — delete the local cnpg cluster"

$(BIN): $(shell find . -name '*.go' -not -path './bin/*')
	@mkdir -p bin
	go build -o $(BIN) ./cmd/klub-lotto

build: $(BIN)

# Fast offline checks — no browser, no network. Mirrors CI.
vet:
	go vet ./...

test:
	go test ./...

# Build + vet + test in one shot. Run before committing.
check: build vet test

doctor: $(BIN)
	$(BIN) doctor

login: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) login

# Default PoC target: solve the quiz with a visible browser, then commit
# the new wiki state and push.
quiz: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) quiz
	@bash scripts/sync.sh

quiz-dry: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) quiz --dry-run

sudoku-dry: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) sudoku --dry-run

sudoku: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) sudoku --submit

ordkloever-dry: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) ordkloever --dry-run $(GAME_PROVIDER_FLAG)

ordkloever-extract: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) ordkloever --dry-run --extract-only

ordkloever: $(BIN)
	@if [ -z "$(GAME_ANSWER)" ]; then \
		echo "No pre-supplied ANSWER; auto-playing Ordkløver (fast model <7/12: $(ORDKLOEVER_FAST); reasoning model >=7/12: $(ORDKLOEVER_REASON); vision: $(OPENROUTER_VISION_MODEL))."; \
		OPENROUTER_VISION_MODEL=$(OPENROUTER_VISION_MODEL) $(LOCAL_BROWSER_ENV) $(BIN) ordkloever --submit --probe-letters --auto-answer --provider "$(ORDKLOEVER_FAST)" --final-provider "$(ORDKLOEVER_REASON)"; \
	else \
		OPENROUTER_VISION_MODEL=$(OPENROUTER_VISION_MODEL) $(LOCAL_BROWSER_ENV) $(BIN) ordkloever --submit --answer "$(GAME_ANSWER)" --provider "$(ORDKLOEVER_FAST)"; \
	fi

ordkloever-probe: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) ordkloever --submit --probe-letters --letter-rounds "$(or $(ROUNDS),3)" $(GAME_AUTO_ANSWER_FLAG) $(GAME_PROVIDER_FLAG)

ordknude-dry: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) ordknude --dry-run $(ORDKNUDE_PROVIDER_FLAG)

ordknude-extract: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) ordknude --dry-run --extract-only

ordknude: $(BIN)
	@if [ -z "$(GAME_ANSWER)" ]; then \
		echo "No pre-supplied ANSWER; auto-playing Ordknuden (model: $(ORDKNUDE_MODEL); propose via LLM + submit real guesses one-by-one from blank sheet until solved; permanent, no do-overs)."; \
		$(LOCAL_BROWSER_ENV) $(BIN) ordknude $(ORDKNUDE_PROVIDER_FLAG); \
	else \
		$(LOCAL_BROWSER_ENV) $(BIN) ordknude --submit --answer "$(GAME_ANSWER)" $(ORDKNUDE_PROVIDER_FLAG); \
	fi

# Krydsord default word/candidate/assembler model. Override with PROVIDER=/WORD_PROVIDER=.
KRYDSORD_PROVIDER := $(or $(PROVIDER),$(provider),$(WORD_PROVIDER),openai/gpt-5.5)

krydsord-dry: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) krydsord --dry-run --provider "$(KRYDSORD_PROVIDER)"

# Stage 1 of the crossword solver: deconstruct the board into a clue graph
# (JSON) via the vision model, verify it (2nd pass on length/direction), exit.
# Try a different vision LLM with GRAPH_MODEL=, e.g.:
#   make krydsord-graph GRAPH_MODEL=openai/gpt-5.4
# Disable the verify pass with VERIFY=false.
KRYDSORD_GRAPH_MODEL := $(or $(GRAPH_MODEL),$(OPENROUTER_VISION_MODEL))
KRYDSORD_VERIFY_FLAG := $(if $(filter false 0 no,$(VERIFY)),--verify=false)
krydsord-graph: $(BIN)
	OPENROUTER_VISION_MODEL=$(KRYDSORD_GRAPH_MODEL) $(LOCAL_BROWSER_ENV) $(BIN) krydsord --graph $(KRYDSORD_VERIFY_FLAG)

# Stage 2: solve from a VALIDATED graph (run `make krydsord-graph` first and
# check it). Builds the board + CSP deterministically (no AI) and solves with the
# reasoning model — no browser/vision, no re-deconstruction. Uses the latest saved
# graph unless GRAPH_FILE=path is given.
# Configure the solver model with SOLVE_MODEL= (or PROVIDER=), e.g.:
#   make krydsord-solve GRAPH_FILE=krydsord_20260613_graph.json SOLVE_MODEL=openai/gpt-5.4
# krydsord-solve-dry builds + saves the board + prompt WITHOUT calling the LLM.
KRYDSORD_SOLVE_PROVIDER := $(or $(SOLVE_MODEL),$(PROVIDER),$(provider),$(WORD_PROVIDER),~google/gemini-pro-latest)
KRYDSORD_GRAPH_FILE_FLAG := $(if $(GRAPH_FILE),--graph-file "$(GRAPH_FILE)")
# SUBMIT=true fills the solved grid onto danskespil.dk and submits (permanent;
# only if the solution validates cleanly against the live mask). On a confirmed
# correct submission the verified clue→answers are recorded in the dictionary.
KRYDSORD_SUBMIT_FLAG := $(if $(filter true 1 yes,$(SUBMIT)),--submit)
# SOLUTION_FILE= re-submits a saved solution JSON without re-running the LLM.
KRYDSORD_SOLUTION_FILE_FLAG := $(if $(SOLUTION_FILE),--solution-file "$(SOLUTION_FILE)")
krydsord-solve: $(BIN)
	OPENROUTER_VISION_MODEL=$(OPENROUTER_VISION_MODEL) $(LOCAL_BROWSER_ENV) $(BIN) krydsord --solve --provider "$(KRYDSORD_SOLVE_PROVIDER)" $(KRYDSORD_GRAPH_FILE_FLAG) $(KRYDSORD_SOLUTION_FILE_FLAG) $(KRYDSORD_SUBMIT_FLAG)

krydsord-solve-dry: $(BIN)
	$(BIN) krydsord --solve --dry-run $(KRYDSORD_GRAPH_FILE_FLAG)

krydsord: $(BIN)
	OPENROUTER_VISION_MODEL=$(OPENROUTER_VISION_MODEL) $(LOCAL_BROWSER_ENV) $(BIN) krydsord --submit --provider "$(KRYDSORD_PROVIDER)"

# Blok for Blok is a Phaser WebGL canvas game with no accessible JS state, so it
# is driven by pixel-perception + real coordinate mouse-drags — now a native Go
# subcommand (internal/klublotto/blok.go), like the other games. It opens the
# game, clicks "Start spil", then plays ON until the board can't take another
# piece (game-over), maximising the score. The first play of the day still earns
# the lod when it crosses 200. Run `make login` first if the session isn't
# authenticated. Pass GOAL=<n> to stop early once the live score reaches n.
BLOK_GOAL := $(or $(GOAL),$(goal),0)
blok: $(BIN)
	$(LOCAL_BROWSER_ENV) $(BIN) blok --goal $(BLOK_GOAL)

wiki-query: $(BIN)
	@$(BIN) wiki query "$(Q)"

wiki-lint: $(BIN)
	$(BIN) wiki lint

sync:
	@bash scripts/sync.sh

reset:
	-agent-browser close --all 2>/dev/null
	@echo "agent-browser daemon(s) stopped. Re-run 'make login' or 'make quiz'."

# Park the session on a blank page when you're done for the day. The danskespil
# klub-lotto tab (live countdown timer + trackers + WebGL games) leaks and pegs a
# renderer if left open overnight — especially across the midnight daily rollover —
# leaving Chrome "unresponsive" by morning. Navigating to about:blank unloads that
# page (freeing the renderer) while keeping the same Chrome/session alive so the
# login persists for tomorrow. Run this after solving the day's puzzles.
park:
	@$(AGENT_BROWSER_BIN) --session $(AGENT_BROWSER_SESSION) open about:blank >/dev/null 2>&1 \
	  && echo "parked: klublotto session tab → about:blank (session kept alive; heavy page unloaded)" \
	  || echo "could not park (is the klublotto session running?)"

clean:
	rm -rf bin/ .klublotto/

# ---------- k8s (docker-desktop) ----------

# --- Image tagging ---------------------------------------------------------
#
# docker-desktop's kubelet caches images by tag, so reusing klub-lotto:dev
# after `docker build` does NOT refresh the running pod — kubectl rollout
# restart will reuse whatever was first pulled under that tag (see the
# painful experience in wiki/concepts/k8s-deploy.md). Tagging each build
# with the current git short SHA sidesteps the cache entirely: the tag is
# new, so the kubelet has to resolve it from the local image store.
#
# Dirty trees get a `-dirty.<timestamp>` suffix so successive iterations
# on the same commit still produce unique tags.

IMAGE_REPO ?= klub-lotto
GIT_SHA    := $(shell git rev-parse --short HEAD 2>/dev/null || echo nogit)
GIT_DIRTY  := $(shell git diff --quiet HEAD -- 2>/dev/null || echo -dirty.$(shell date -u +%Y%m%d%H%M%S))
IMAGE_TAG  ?= $(GIT_SHA)$(GIT_DIRTY)
IMAGE      := $(IMAGE_REPO):$(IMAGE_TAG)

# Run before the first `make image` if you haven't pulled deps yet.
tidy:
	go mod tidy

# Builds with both the git-sha tag and the floating :dev tag. The :dev tag
# is what the YAML in deploy/k8s/40-deployment.yaml ships with; the sha
# tag is what `make deploy` uses to force a fresh pull.
image: tidy
	@echo "Building $(IMAGE) (also tagging :dev)"
	docker build -f deploy/Dockerfile -t $(IMAGE) -t $(IMAGE_REPO):dev .
	@echo "Built $(IMAGE)"

# Applies every manifest in deploy/k8s/. The 20-secret-env.yaml file is
# gitignored — copy 20-secret-env.example.yaml to 20-secret-env.yaml and
# fill in the real keys before running this for the first time.
k8s-up:
	@if [ ! -f deploy/k8s/20-secret-env.yaml ]; then \
	  echo "deploy/k8s/20-secret-env.yaml missing — copy the .example and fill it in"; exit 1; fi
	kubectl apply -f deploy/k8s/

# Build a new image with a git-sha tag and roll the deployment to it.
# This is the day-to-day "ship my changes" target. It assumes namespace
# and cnpg are already up from a previous `make k8s-up`.
deploy: image
	@echo "Setting deploy/klub-lotto image to $(IMAGE)"
	kubectl -n klub-lotto set image deploy/klub-lotto app=$(IMAGE)
	kubectl -n klub-lotto rollout status deploy/klub-lotto --timeout=180s

k8s-down:
	kubectl delete namespace klub-lotto --ignore-not-found

k8s-logs:
	kubectl -n klub-lotto logs -f deploy/klub-lotto

port-forward:
	kubectl -n klub-lotto port-forward svc/klub-lotto 8080:80

ui-url:
	@echo "Ingress:        http://klub-lotto.localhost"
	@echo "Port-forward:   http://localhost:8080  (run: make port-forward)"

db-shell:
	# Inside the cnpg pod the local socket uses peer auth, which maps the OS
	# user (postgres) — connect as the superuser and target the klublotto db.
	kubectl -n klub-lotto exec -it klublotto-db-1 -- psql -U postgres -d klublotto

# ── Local-dev database (CloudNativePG) ───────────────────────────────────────
# Brings up a single-instance, backup-less klublotto Postgres for syncing and
# inspecting the ledger locally. Requires the cnpg operator to be installed
# cluster-wide (see deploy/k8s/10-cnpg-cluster.yaml header for the one-liner).
db-up:
	kubectl apply -f deploy/k8s/00-namespace.yaml
	kubectl apply -f deploy/k8s/dev-db.yaml
	@echo "Waiting for klublotto-db to become ready (up to 180s)..."
	kubectl -n klub-lotto wait --for=condition=Ready cluster/klublotto-db --timeout=180s
	@echo "DB ready. Sync the wiki ledger with: make db-import"

db-down:
	kubectl -n klub-lotto delete cluster/klublotto-db --ignore-not-found

# Port-forward the read-write Postgres service to localhost:5432 (foreground).
db-port-forward:
	kubectl -n klub-lotto port-forward svc/klublotto-db-rw 5432:5432

# Import wiki/daily/*.md into Postgres (DB becomes the source of truth).
# Port-forwards the cnpg rw service, reads the cnpg-managed app password, runs
# the importer, then tears the port-forward down.
db-import: $(BIN)
	@kubectl -n klub-lotto port-forward svc/klublotto-db-rw 5432:5432 >/tmp/klublotto-db-pf.log 2>&1 & \
	  PF_PID=$$!; \
	  trap 'kill $$PF_PID 2>/dev/null' EXIT; \
	  sleep 4; \
	  PW=$$(kubectl -n klub-lotto get secret klublotto-db-app -o jsonpath='{.data.password}' | base64 -d); \
	  if [ -z "$$PW" ]; then echo "could not read klublotto-db-app password (is the DB up? run: make db-up)"; exit 1; fi; \
	  DATABASE_URL="postgres://klublotto:$$PW@localhost:5432/klublotto?sslmode=disable" $(BIN) wiki import-db
