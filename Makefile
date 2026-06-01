# klub-lotto — convenience targets.
#
# All commands assume the user has Go, agent-browser, and (optionally) qmd
# on PATH. Run `make doctor` to verify.

BIN := bin/klub-lotto

.PHONY: help build doctor login quiz quiz-dry ordknude wiki-query wiki-lint sync clean reset \
        image deploy k8s-up k8s-down k8s-logs port-forward db-shell ui-url tidy

help:
	@echo "make build       — build the CLI into ./bin/klub-lotto"
	@echo "make doctor      — sanity-check config, providers, agent-browser"
	@echo "make login       — interactive login (headed; saves session)"
	@echo "make quiz        — solve today's Quiz (headed), commit wiki, push"
	@echo "make quiz-dry    — same but doesn't click or sync; just shows what we would do"
	@echo "make ordknude    — solve today's Ordknuden with Gemini (headed)"
	@echo "make wiki-query Q='...'  — search the wiki via qmd (or grep)"
	@echo "make sync        — commit wiki/doc changes and push to origin"
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

$(BIN): $(shell find . -name '*.go' -not -path './bin/*')
	@mkdir -p bin
	go build -o $(BIN) ./cmd/klub-lotto

build: $(BIN)

doctor: $(BIN)
	$(BIN) doctor

login: $(BIN)
	$(BIN) login

# Default PoC target: solve the quiz with a visible browser, then commit
# the new wiki state and push.
quiz: $(BIN)
	$(BIN) quiz
	@bash scripts/sync.sh

quiz-dry: $(BIN)
	$(BIN) quiz --dry-run

ordknude: $(BIN)
	$(BIN) ordknude

wiki-query: $(BIN)
	@$(BIN) wiki query "$(Q)"

wiki-lint: $(BIN)
	$(BIN) wiki lint

sync:
	@bash scripts/sync.sh

reset:
	-agent-browser close --all 2>/dev/null
	@echo "agent-browser daemon(s) stopped. Re-run 'make login' or 'make quiz'."

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
	kubectl -n klub-lotto exec -it klublotto-db-1 -- psql -U klublotto klublotto
