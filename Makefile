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

.PHONY: macos
macos: ## Build a universal macOS binary (Intel + Apple silicon)
	@mkdir -p $(DIST)
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/mkr-darwin-amd64 ./cmd/mkr
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/mkr-darwin-arm64 ./cmd/mkr
	@# lipo produces one binary that runs natively on both architectures, so
	@# a single download works on any Mac.
	@if command -v lipo >/dev/null 2>&1; then \
		lipo -create -output $(DIST)/mkr-darwin-universal \
			$(DIST)/mkr-darwin-amd64 $(DIST)/mkr-darwin-arm64 && \
		echo "built universal binary:" && lipo -info $(DIST)/mkr-darwin-universal; \
	else \
		echo "lipo unavailable; per-architecture binaries only"; \
	fi

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
	cp README.md LICENSE NOTICE $(DIST)/bundle/
	mkdir -p $(DIST)/bundle/docs
	cp docs/INSTALLATION.md docs/CONFIGURATION.md docs/USAGE.md docs/SECURITY.md docs/SKILLS.md $(DIST)/bundle/docs/
	cp deploy/NETWORK-REQUIREMENT.md $(DIST)/bundle/
	mkdir -p $(DIST)/bundle/examples
	cp -R examples/skills $(DIST)/bundle/examples/
	printf '{\n  "endpoint": "http://CHANGE-ME:8000",\n  "mode": "approve",\n  "redact": true\n}\n' > $(DIST)/bundle/config.json
	cd $(DIST)/bundle && shasum -a 256 $$(find . -type f | sed 's|^\./||' | sort) > SHA256SUMS
	cd $(DIST) && rm -f mkr-windows-amd64-$(VERSION).zip && zip -qr mkr-windows-amd64-$(VERSION).zip bundle
	@echo "built $(DIST)/mkr-windows-amd64-$(VERSION).zip"
	@cat $(DIST)/bundle/SHA256SUMS

.PHONY: bundle-macos
bundle-macos: macos ## Produce the macOS bundle
	@rm -rf $(DIST)/bundle-macos && mkdir -p $(DIST)/bundle-macos
	@if [ -f $(DIST)/mkr-darwin-universal ]; then \
		cp $(DIST)/mkr-darwin-universal $(DIST)/bundle-macos/mkr; \
	else \
		cp $(DIST)/mkr-darwin-amd64 $(DIST)/bundle-macos/mkr; \
	fi
	chmod +x $(DIST)/bundle-macos/mkr
	cp README.md LICENSE NOTICE $(DIST)/bundle-macos/
	mkdir -p $(DIST)/bundle-macos/docs
	cp docs/INSTALLATION.md docs/CONFIGURATION.md docs/USAGE.md docs/SECURITY.md docs/SKILLS.md docs/TESTING-LOCALLY.md $(DIST)/bundle-macos/docs/
	mkdir -p $(DIST)/bundle-macos/examples
	cp -R examples/skills $(DIST)/bundle-macos/examples/
	cd $(DIST)/bundle-macos && shasum -a 256 $$(find . -type f | sed 's|^\./||' | sort) > SHA256SUMS
	cd $(DIST) && rm -f mkr-darwin-$(VERSION).tar.gz && tar czf mkr-darwin-$(VERSION).tar.gz -C bundle-macos .
	@echo "built $(DIST)/mkr-darwin-$(VERSION).tar.gz"

.PHONY: bundle-all
bundle-all: bundle-windows bundle-macos ## Build every release bundle

.PHONY: clean
clean: ## Remove build output
	rm -rf $(DIST)

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
