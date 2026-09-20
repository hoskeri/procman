GO     ?= go
GOOS   ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)

# All binaries — production tools and the trebuchet test-fixture app — are
# built exactly once into a platform-scoped output directory. Integration
# tests locate them via PROCMAN_BIN instead of building their own copies.
OUT      := $(CURDIR)/_output/$(GOOS)_$(GOARCH)/bin
PROCMAN  := $(OUT)/procman
TREBUCHET := $(OUT)/trebuchet

.PHONY: build test clean

build: $(PROCMAN) $(TREBUCHET)

$(PROCMAN): $(shell find cmd pkg -name '*.go') go.mod go.sum
	@mkdir -p $(OUT)
	$(GO) build -o $(PROCMAN) ./cmd/procman

$(TREBUCHET): $(shell find tools/trebuchet -name '*.go') go.mod go.sum
	@mkdir -p $(OUT)
	$(GO) build -o $(TREBUCHET) ./tools/trebuchet

# Build everything once, then run unit and integration tests. PROCMAN_BIN
# points the integration tests (pkg tests/.../*_test.go) at the prebuilt
# procman binary; without it they skip, so a bare `go test ./...` still works.
test: build
	@PROCMAN_BIN=$(PROCMAN) $(GO) test -count=1 -v ./...

clean:
	@rm -rf _output ./procman ./tools/trebuchet/trebuchet
