.PHONY: build test e2e lint vuln ci install tidy clean

GO := GOTOOLCHAIN=auto go

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

# Drives the built binary across real processes: queue order, crash recovery,
# signals. Uses the real per-user queue, so it waits behind any run in
# progress, and checks the budget scope where systemd can enforce one.
e2e:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(GO) build -o "$$tmp/qrun" ./cmd/qrun && ./scripts/e2e.sh "$$tmp/qrun"

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed."; \
		echo "  install: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	golangci-lint run

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Run before pushing.
ci: build test lint vuln e2e

install:
	$(GO) install ./cmd/qrun

tidy:
	$(GO) mod tidy

clean:
	$(GO) clean ./...
