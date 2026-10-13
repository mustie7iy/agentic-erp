.PHONY: generate run-server build lint lint-all test test-short db-up test-db coverage fuzz perf load soak resilience demo clean

build:
	go build -o bin/erp ./cmd/erp
	go build -o bin/erp-server ./cmd/erp-server
	go build -o bin/erp-router ./cmd/erp-router

# Structural lint (.golangci.yml) of code changed since LINT_BASE: the merge
# base with main locally, the pushed range in CI. lint-all shows the whole tree.
GOLANGCI = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
LINT_BASE ?= main

lint:
	$(GOLANGCI) run --new-from-merge-base=$(LINT_BASE) ./...

lint-all:
	$(GOLANGCI) run ./...

# Everything: functional, end-to-end and non-functional suites, race detector on.
test:
	go vet ./...
	go test -race -count=1 ./...

# Fast loop: skips performance budgets.
test-short:
	go test -short ./...

# Whole-suite statement coverage of production code, with the CI minimum.
# Schema tests need PostgreSQL. `make db-up` starts one in Docker on port 55432.
DATABASE_URL ?= postgres://postgres:dev@localhost:55432/postgres?sslmode=disable

db-up:
	docker start erp-pg 2>/dev/null || docker run -d --name erp-pg -e POSTGRES_PASSWORD=dev -p 55432:5432 postgres:17

test-db:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./db

# Regenerate the server and client from the API contract (ADR-0014).
generate:
	cd api && go generate ./...

# Run the API server against the local database (see internal/platform/config).
run-server:
	DATABASE_URL="$(DATABASE_URL)" ERP_MIGRATE_ON_START=true go run ./cmd/erp-server

coverage:
	DATABASE_URL="$(DATABASE_URL)" ./scripts/coverage.sh

FUZZTIME ?= 15s
FUZZ_TARGETS = money:FuzzParseDecimal config:FuzzParseValidateDiff analytics:FuzzNarrative \
	bankfile:FuzzPain001Text bankfile:FuzzValidIBAN sap:FuzzODataValue

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
	go test ./internal/nfr -run Perf -v
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 ./internal/ledger ./internal/payments -run Budget -v
	go test ./internal/nfr -run '^$$' -bench . -benchmem

# Non-functional suites against the database (05-scalability §5.8). Each
# takes ERP_LOAD_USERS (16) and ERP_LOAD_RPS (0: unpaced); load takes
# ERP_LOAD_DURATION (10s), soak takes ERP_SOAK_DURATION (30s) and
# ERP_SOAK_WINDOW (5s). The nightly workflow runs them long.
load:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./internal/nfr -run 'TestLoad'
soak:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v -timeout 48h ./internal/nfr -run 'TestSoak'
resilience:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./internal/nfr -run 'TestResilience'

# Full walkthrough through the CLI against a real API server and database:
# mock SAP extract, configuration draft, migration of company 1000 through
# the integration API, the invoice rule, the four agents (scripted model)
# and analytics. Needs `make db-up`.
demo:
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 -v ./cmd/erp -run TestEndToEnd

clean:
	rm -rf bin data
