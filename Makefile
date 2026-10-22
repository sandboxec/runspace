# Development tasks; make help lists them. Releases are built by goreleaser
# from a pushed v* tag, and snapshot builds the same archives locally.

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
GORELEASER    ?= goreleaser

# The systems with their own source files; vet and lint check each one.
OSES := linux darwin windows

# The cli/cli release that internal/csconn was copied from.
UPSTREAM_TAG ?= v2.100.0

# Selects live checks by name, as in make live RUN=TestExitCodes.
RUN ?= .

# Any value makes live also wait out the idle timeouts, as in make live
# LONG=1. That adds 15 to 30 minutes.
LONG ?=

.PHONY: all help build install check fmt fmt-check vet lint test tidy \
	tidy-check live canary token snapshot licenses upstream-diff clean

all: check build ## Run every check, then build

help: ## List the targets
	@awk -F ':.*## ' '/^[a-z-]+:.*## / { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# build and install use CGO_ENABLED=0, as releases do.
build: ## Build build/bin/runspace
	CGO_ENABLED=0 $(GO) build -o build/bin/ .

install: ## Install runspace with go install
	CGO_ENABLED=0 $(GO) install .

check: fmt-check vet lint test tidy-check ## Run every check that needs no token

fmt: ## Format every Go file
	gofmt -w .

fmt-check: ## Fail if a Go file is not formatted
	@files=$$(gofmt -l .) || exit; \
	if [ -n "$$files" ]; then echo "gofmt -w needed:" >&2; echo "$$files" >&2; exit 1; fi

vet: ## Vet every package, live checks included, for each OS
	for os in $(OSES); do GOOS=$$os $(GO) vet -tags live ./... || exit; done

lint: ## Run golangci-lint with every linter, for each OS
	for os in $(OSES); do GOOS=$$os $(GOLANGCI_LINT) run --default=all ./... || exit; done

test: ## Run the unit tests with the race detector
	$(GO) test -race ./...

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

tidy-check: ## Fail if go.mod or go.sum is not tidy
	$(GO) mod tidy -diff

# live runs the live checks, under -short unless LONG is set. Besides
# RUNSPACE_TOKEN it needs an authenticated gh, and Docker for the isolation
# check. It creates billable codespaces and deletes every codespace in
# RUNSPACE_LIVE_REPO, which defaults to the checks' own scratch repository.
live: token ## Run the live checks (billable codespaces); LONG=1 adds the idle waits
	$(GO) test -tags live -count=1 $(if $(LONG),,-short) -timeout 90m \
		-run '$(RUN)' ./test/live

# canary runs the exit-code, termination, and MCP checks against a scratch
# repository, to catch drift in the undocumented protocol. It needs
# RUNSPACE_TOKEN, RUNSPACE_LIVE_REPO, and gh with GH_TOKEN for cleanup, and
# it creates billable codespaces.
canary: token ## Run the weekly drift checks (billable codespaces)
	$(GO) test -tags live -count=1 -short -timeout 40m \
		-run 'TestExitCodes|TestTimeoutTerminatesRemoteCommand|TestMCPSession' ./test/live

# token stops live and canary early, before any build, when RUNSPACE_TOKEN
# is unset. It never prints the token.
token:
	@test -n "$$RUNSPACE_TOKEN" || { \
		echo 'set RUNSPACE_TOKEN, for example to "$$(gh auth token)"' >&2; exit 2; }

snapshot: ## Build every release archive into dist/, publishing nothing
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean

licenses: ## Check third-party licenses and save them in build/licenses
	scripts/licenses.sh

upstream-diff: ## Compare internal/csconn with cli/cli UPSTREAM_TAG
	scripts/upstream-diff.sh $(UPSTREAM_TAG)

clean: ## Remove build/ and dist/
	rm -rf build dist
