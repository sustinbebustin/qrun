.PHONY: build test e2e lint vuln release-check ci install tidy clean

GO := GOTOOLCHAIN=auto go

# The GoReleaser that release-check runs; release.yml pins the same version.
GORELEASER_VERSION := v2.19.0
GORELEASER := $(GO) run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

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

# Validate .goreleaser.yaml, then run a snapshot release into dist/: the
# archives, checksums and Homebrew cask, with nothing published.
release-check:
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean --skip=publish

# Mirror of the .github/workflows/ci.yml jobs. Run before pushing.
ci: build test lint vuln e2e release-check

install:
	$(GO) install ./cmd/qrun

tidy:
	$(GO) mod tidy

clean:
	$(GO) clean ./...
