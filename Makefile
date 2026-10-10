.PHONY: generate run-server build test test-short db-up test-db coverage fuzz perf demo demo-analytics clean

build:
	go build -o bin/erp ./cmd/erp

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
	bankfile:FuzzPain001Text bankfile:FuzzValidIBAN sap:FuzzODataValue capture:FuzzCapture

# Fuzz every untrusted-input parser for FUZZTIME each.
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; echo "== $$pkg $$fn"; \
		go test ./internal/$$pkg -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) || exit 1; \
	done

# Performance budgets and benchmarks.
perf:
	go test ./internal/nfr -run Perf -v
	DATABASE_URL="$(DATABASE_URL)" go test -count=1 ./internal/ledger -run Budget -v
	go test ./internal/nfr -run '^$$' -bench . -benchmem

# Full walkthrough on mock SAP data: migrate company 1000, run the invoice flow.
demo: build
	rm -rf data
	./bin/erp gen-mock-sap
	./bin/erp migrate -company 1000
	./bin/erp invoices
	./bin/erp tasks
	./bin/erp audit-verify

# Two years of mock SAP data, migrated, then an on-demand dashboard.
demo-analytics: build
	rm -rf data
	./bin/erp gen-mock-sap -year 2025 -out data/sap-2025.json
	./bin/erp gen-mock-sap -year 2026 -through 9 -out data/sap-2026.json
	./bin/erp migrate -source file:data/sap-2025.json -year 2025 -company 1000,2000
	./bin/erp migrate -source file:data/sap-2026.json -year 2026 -company 1000,2000
	./bin/erp metrics
	./bin/erp query -measures revenue,operating_margin -by fiscal_year -where "fiscal_month <= 9" -companies 1000
	./bin/erp report -file examples/monthly-sales.json -companies 1000 -as sales.director

clean:
	rm -rf bin data
