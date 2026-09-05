# ruyishell - build/test/release Makefile.
# The single shipped binary is `rysh` (package ./cmd/rysh).

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG      := ./cmd/rysh
BIN      := rysh
BUILDDIR := bin
DISTDIR  := dist
LDFLAGS  := -X main.version=$(VERSION)
GOFLAGS  := -trimpath
CGO      := 0

.PHONY: all build test vet fmt fmtcheck race cross install clean

all: build

# Build the local binary (static, no cgo) into ./bin/rysh.
build:
	CGO_ENABLED=$(CGO) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILDDIR)/$(BIN) $(PKG)

# Run the full test suite.
test:
	go test ./...

# go vet across all packages.
vet:
	go vet ./...

# Format all Go sources in place.
fmt:
	gofmt -s -w .

# Check formatting without writing (CI uses this).
fmtcheck:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then \
		echo "gofmt needs to run on:"; echo "$$out"; exit 1; \
	else echo "gofmt: clean"; fi

# Race detector on the whole suite.
race:
	go test -race ./...

# Cross-compile the release matrix into ./dist. Mirrors the goreleaser targets.
cross: clean
	@mkdir -p $(DISTDIR)
	@for target in \
		linux/amd64 linux/arm64 linux/386 \
		darwin/amd64 darwin/arm64 \
		windows/amd64 windows/arm64 windows/386; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=$(DISTDIR)/$(BIN)-$$os-$$arch; \
		[ $$os = windows ] && out=$$out.exe; \
		echo "  -> $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=$(CGO) \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
	@cd $(DISTDIR) && sha256sum $(BIN)-* > checksums.txt
	@echo "cross-build complete in $(DISTDIR)/"

# Package the cross-built matrix into per-platform archives: .tar.gz for
# Unix, .zip for Windows (falls back to tar.gz when zip is missing). Each
# archive holds the single binary plus checksums.txt for post-download
# verification.
package: cross
	@mkdir -p $(DISTDIR)/pkg
	@cp scripts/install.sh $(DISTDIR)/pkg/install.sh
	@cd $(DISTDIR) && \
	for f in $(BIN)-linux-* $(BIN)-darwin-*; do \
		[ -f "$$f" ] || continue; \
		tar -czf pkg/$${f}.tar.gz "$$f" checksums.txt; \
	done
	@cd $(DISTDIR) && \
	for f in $(BIN)-windows-*.exe; do \
		[ -f "$$f" ] || continue; \
		name="$${f%.exe}"; \
		if command -v zip >/dev/null 2>&1; then \
			zip -q pkg/$${name}.zip $$f checksums.txt; \
		else \
			echo "  zip not found; using tar.gz for $$name"; \
			tar -czf pkg/$${name}.tar.gz $$f checksums.txt; \
		fi; \
	done
	@echo "archives in $(DISTDIR)/pkg/:"
	@ls -1 $(DISTDIR)/pkg/
	@echo "attach these as release assets"

# release: full test + cross-build + package for a tagged version.
release:
	$(MAKE) test
	$(MAKE) cross VERSION=$(VERSION)
	$(MAKE) package VERSION=$(VERSION)

# Install the binary into $$GOPATH/bin via go install.
install:
	CGO_ENABLED=$(CGO) go install -ldflags "$(LDFLAGS)" $(PKG)

clean:
	rm -rf $(BUILDDIR) $(DISTDIR)
