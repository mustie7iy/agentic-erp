.PHONY: build test demo clean

build:
	go build -o bin/erp ./cmd/erp

test:
	go vet ./...
	go test -race ./...

# Full walkthrough on mock SAP data: migrate company 1000, run the invoice flow.
demo: build
	rm -rf data
	./bin/erp gen-mock-sap
	./bin/erp migrate -company 1000
	./bin/erp invoices
	./bin/erp tasks
	./bin/erp audit-verify

clean:
	rm -rf bin data
