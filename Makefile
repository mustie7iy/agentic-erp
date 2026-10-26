.PHONY: temporal-checks generate run-server build images images-reproducible image-scan test-image-scripts signing-roundtrip test-release-scripts lint lint-all test test-short ci-local db-up test-db coverage fuzz perf load soak resilience failover restore-drill demo walkthrough clean

build:
	go build -o bin/erp ./cmd/erp
	go build -o bin/erp-server ./cmd/erp-server
	go build -o bin/erp-router ./cmd/erp-router
	go build -o bin/erp-web ./cmd/erp-web
	# erp-analytics links DuckDB through cgo (ADR-0044): built with the host's
	# C toolchain; its image is built with the others (make images).
	CGO_ENABLED=1 go build -o bin/erp-analytics ./cmd/erp-analytics

# Container images of the five binaries (ADR-0032), not pushed: digests in
# dist/images/images.txt, SBOMs in dist/images/sbom. ko builds erp-server,
# erp-router, erp-web and erp (.ko.yaml); docker buildx builds erp-analytics (cgo,
# DuckDB: not ko's, ADR-0044) from deploy/images/erp-analytics.Dockerfile,
# one platform at a time, since each compiles api/gen in a few GB of the
# Docker VM's memory (KO_PLATFORMS=linux/arm64 builds one platform of
# every image). images-reproducible builds twice with private caches and
# builders and fails unless the digests match (CI's gate); image-scan runs
# grype on the SBOMs and the bases; test-image-scripts checks the scripts
# with fake tools.
images:
	./scripts/images.sh dist/images

images-reproducible:
	./scripts/images-reproducible.sh dist/images

image-scan:
	./scripts/image-scan.sh dist/images

test-image-scripts:
	./scripts/test-images.sh

# Signing round trip against a throwaway registry (SIGNING_REGISTRY): push,
# sign and attest with a key pair made for the run, verify, and check the
# refusals (scripts/signing-roundtrip.sh). Releases sign with the KMS key:
# scripts/sign-images.sh and scripts/verify-image.sh (ADR-0032).
SIGNING_REGISTRY ?= localhost:5000/erp
signing-roundtrip:
	./scripts/signing-roundtrip.sh $(SIGNING_REGISTRY)

# The release version rules and changelog (scripts/next-version.sh,
# cliff.toml) on throwaway repositories; GIT_CLIFF names git-cliff, without
# it the changelog cases are skipped. The release itself is
# .github/workflows/release.yml.
test-release-scripts:
	./scripts/test-release-scripts.sh

# Structural lint (.golangci.yml). lint checks code changed since LINT_BASE,
# the merge base with main: the fast local loop. lint-all checks the whole
# tree, which is at zero issues; CI runs it.
# api/gen is left out of the package list: its findings are excluded anyway,
# and analysing its ~610,000 generated lines takes over half an hour.
GOLANGCI ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
LINT_BASE ?= main
LINT_FLAGS ?=
LINT_PKGS = $(shell go list -e -f '{{.Dir}}' ./... | grep -vE '/api/gen(/|$$)')

lint:
	$(GOLANGCI) run $(LINT_FLAGS) --new-from-merge-base=$(LINT_BASE) $(LINT_PKGS)

lint-all:
	$(GOLANGCI) run $(LINT_FLAGS) $(LINT_PKGS)

# Everything: functional, end-to-end and non-functional suites, race detector on.
test:
	go vet ./...
	go test -race -count=1 ./...

# Fast loop: skips performance budgets.
test-short:
	go test -short ./...

# The full gates before a pull request's short GitHub check (06 §6.5):
# CI's tools at its pinned versions, the shuffled race runs, budgets,
# coverage and fuzzing. Its report is what scripts/merge-pr.sh requires.
ci-local:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/ci-local.sh

# Whole-suite statement coverage of production code, with the CI minimum.
# Schema tests need PostgreSQL. `make db-up` starts one in Docker on port 55432.
DATABASE_URL ?= postgres://postgres:dev@localhost:55432/postgres?sslmode=disable

# The workflow engine's module (temporal/, ADR-0035): vet, lint, tests with
# -race three times shuffled, and 100% coverage.
temporal-checks:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/temporal-checks.sh

db-up:
	docker start erp-pg 2>/dev/null || docker run -d --name erp-pg -e POSTGRES_PASSWORD=dev -p 55432:5432 postgres:17

test-db:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./db

# Regenerate the server and client from the API contract (ADR-0014) and the
# web workspace's templ components (ADR-0052); CI fails on any difference.
generate:
	cd api && go generate ./...
	go generate ./internal/web/...

# Run the API server against the local database (see internal/platform/config).
# ERP_ENV has no default; a local run is development unless the caller says otherwise.
ERP_ENV ?= development
run-server:
	DATABASE_URL="$(DATABASE_URL)" ERP_ENV="$(ERP_ENV)" ERP_MIGRATE_ON_START=true go run ./cmd/erp-server

coverage:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/coverage.sh

FUZZTIME ?= 15s
FUZZ_TARGETS = analytics/reader:FuzzManifestPath money:FuzzParseDecimal money:FuzzConvert config:FuzzParseValidateDiff countrypack:FuzzParseValidate analytics:FuzzNarrative \
	bankfile:FuzzPain001Text bankfile:FuzzValidIBAN bankstatement:FuzzParseCamt053 bankstatement:FuzzParseMT940 sap:FuzzODataValue sap:FuzzODataString \
	platform/redact:FuzzText platform/webhook:FuzzVerify platform/extensions:FuzzCompile platform/rules:FuzzCompile platform/cabank:FuzzParse tax:FuzzCalculate

# Evaluate one agent live (ADR-0039): make eval AGENT=ap_exceptions (the
# migration suite needs the cell started with ERP_EXTRACT_DIR=evals/migration). It
# spends model money on the local cell's gateway (ERP_CONTROL_URL,
# ERP_API_URL, ERP_ADMIN_URL with ERP_DEV_ISSUER), so it runs on demand and
# never in CI, which replays the suites with the scripted model instead.
eval:
	@test -n "$(AGENT)" || { echo "make eval AGENT=<agent type>"; exit 2; }
	go run ./cmd/erp eval run -suite evals/$(AGENT)/suite.yaml

# Fuzz every untrusted-input parser for FUZZTIME each. A run that fails
# without saving a failing input under testdata/fuzz is the fuzz engine
# timing out as its budget ends ("context deadline exceeded"), not a
# finding: it is run once more. A saved input always fails the target.
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; dir=internal/$$pkg/testdata/fuzz/$$fn; echo "== $$pkg $$fn"; \
		before=$$(ls $$dir 2>/dev/null | wc -l); \
		go test ./internal/$$pkg -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) && continue; \
		after=$$(ls $$dir 2>/dev/null | wc -l); \
		if [ "$$after" != "$$before" ]; then echo "failing input saved in $$dir"; exit 1; fi; \
		echo "== $$pkg $$fn: no failing input saved; the engine timed out, running once more"; \
		go test ./internal/$$pkg -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) || exit 1; \
	done

# Performance budgets and benchmarks.
perf:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/budgets.sh -v
	go test ./internal/nfr -run '^$$' -bench . -benchmem

# Non-functional suites against the database (05-scalability §5.8). Each
# takes ERP_LOAD_USERS (16) and ERP_LOAD_RPS (0: unpaced); load takes
# ERP_LOAD_DURATION (10s), soak takes ERP_SOAK_DURATION (30s) and
# ERP_SOAK_WINDOW (5s). The weekly workflow runs them long.
load:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./internal/nfr -run 'TestLoad'
soak:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v -timeout 48h ./internal/nfr -run 'TestSoak'
resilience:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./internal/nfr -run 'TestResilience'
# A replica promoted behind PgBouncer under load (ADR-0040): its own
# PostgreSQL primary, replica and PgBouncer in Docker; weekly in the nfr job.
failover:
	scripts/failover.sh

# The restore drill in Docker (ADR-0040): a base backup and the WAL archive
# restored to a point in time and verified. RUN_URL is where its log is.
RUN_URL ?= https://localhost/restore-drill
restore-drill:
	RESTORE_PROVIDER=local scripts/restore-drill.sh "$(RUN_URL)"

# Full walkthrough through the CLI against a real API server and database:
# mock SAP extract, configuration draft, migration of company 1000 through
# the integration API, the invoice rule, the four agents (scripted model)
# and analytics. Needs `make db-up`.
demo:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./cmd/erp -run TestEndToEnd

# The pilot walkthrough for the product owner: a Canadian company's story,
# narrated step by step against the real erp-server and erp, on
# DATABASE_URL (scripts/walkthrough.sh). WALKTHROUGH_PAUSE=1 waits after
# each step; WALKTHROUGH_KEEP=1 keeps the server up at the end. Needs
# `make db-up` and jq.
walkthrough:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/walkthrough.sh

clean:
	rm -rf bin data dist
