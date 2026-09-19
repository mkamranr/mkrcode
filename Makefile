# mkr build and bundling.
#
# The client must ship as a single file to air-gapped Windows workstations,
# so every target below builds a static, dependency-free binary.

VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST    := dist

# CGO is disabled everywhere: it is what makes the binary static, keeps the
# build reproducible, and removes any dependency on a C toolchain or on the
# glibc version of the target machine.
export CGO_ENABLED = 0

.PHONY: all
all: test build

.PHONY: build
build: ## Build for the host platform
	@mkdir -p $(DIST)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/mkr ./cmd/mkr

.PHONY: windows
windows: ## Build mkr.exe for windows/amd64
	@mkdir -p $(DIST)
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/mkr.exe ./cmd/mkr

.PHONY: linux
linux: ## Build for linux/amd64
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/mkr-linux-amd64 ./cmd/mkr

.PHONY: test
test: ## Run the full test suite
	go test ./...

.PHONY: race
race: ## Run tests with the race detector
	CGO_ENABLED=1 go test -race ./...

.PHONY: cover
cover: ## Report test coverage per package
	go test -cover ./...

.PHONY: vet
vet: ## Run go vet, including the dev-only tools
	go vet ./...
	go vet -tags devtools ./devtools/...

.PHONY: check
check: vet test lint-egress ## Everything CI should run

# The egress restriction is only meaningful if netguard supplies the only
# HTTP client in the binary. This check fails the build if any other package
# constructs a transport or uses the default one.
.PHONY: lint-egress
lint-egress: ## Fail if any package outside netguard builds an HTTP client
	@echo "checking that netguard is the only source of HTTP clients..."
	@! grep -rn --include='*.go' \
		-e 'http\.DefaultClient' \
		-e 'http\.DefaultTransport' \
		-e '&http\.Transport{' \
		-e '&http\.Client{' \
		internal cmd \
		| grep -v '^internal/netguard/' \
		| grep -v '_test\.go:' \
		|| (echo "ERROR: an HTTP client is built outside internal/netguard; egress would not be restricted" && exit 1)
	@echo "ok"

.PHONY: mockserver
mockserver: ## Build the dev-only mock vLLM server
	@mkdir -p $(DIST)
	go build -tags devtools -o $(DIST)/mockserver ./devtools/mockserver

.PHONY: bundle-windows
bundle-windows: windows ## Produce the Windows client bundle for removable media
	@rm -rf $(DIST)/bundle && mkdir -p $(DIST)/bundle
	cp $(DIST)/mkr.exe $(DIST)/bundle/
	cp README.md $(DIST)/bundle/
	mkdir -p $(DIST)/bundle/docs
	cp docs/INSTALLATION.md docs/CONFIGURATION.md docs/USAGE.md docs/SECURITY.md $(DIST)/bundle/docs/
	cp deploy/NETWORK-REQUIREMENT.md $(DIST)/bundle/
	printf '{\n  "endpoint": "http://CHANGE-ME:8000",\n  "mode": "approve",\n  "redact": true\n}\n' > $(DIST)/bundle/config.json
	cd $(DIST)/bundle && shasum -a 256 $$(find . -type f | sed 's|^\./||' | sort) > SHA256SUMS
	cd $(DIST) && rm -f mkr-windows-amd64-$(VERSION).zip && zip -qr mkr-windows-amd64-$(VERSION).zip bundle
	@echo "built $(DIST)/mkr-windows-amd64-$(VERSION).zip"
	@cat $(DIST)/bundle/SHA256SUMS

.PHONY: clean
clean: ## Remove build output
	rm -rf $(DIST)

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
