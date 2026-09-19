VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# CGO_ENABLED=0 is not an optimization. Section 3's promise is "copy one file,
# run one systemd unit", and a binary linked against the host's libc is not one
# file -- it is one file plus whatever the build machine happened to have.
GOBUILD := CGO_ENABLED=0 go build

BIN := bin

.PHONY: all
all: test build

.PHONY: build
build: $(BIN)/orchd $(BIN)/orch

$(BIN)/orchd: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	@mkdir -p $(BIN)
	$(GOBUILD) -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/orchd

$(BIN)/orch: $(shell find . -name '*.go' -not -name '*_test.go') go.mod
	@mkdir -p $(BIN)
	$(GOBUILD) -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/orch

# -race because most of what this tool does is concurrent: leases, heartbeats,
# the outbox, the SSE bus. -count=1 because a cached pass proves nothing.
# The race detector needs cgo, so tests run with it on; the shipped binaries
# above are still static.
.PHONY: test
test:
	CGO_ENABLED=1 go test -race -count=1 ./...

.PHONY: test-short
test-short:
	go test -count=1 ./...

.PHONY: cover
cover:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	go vet ./...
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
	  echo 'not gofmt-clean:'; echo "$$unformatted"; exit 1; fi

.PHONY: check
check: vet test

# Cross-compiled, because the machine that runs deployments is often not the
# machine that builds them.
.PHONY: release
release:
	@mkdir -p $(BIN)
	for os in linux darwin; do \
	  for arch in amd64 arm64; do \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
	      -o $(BIN)/orchd-$$os-$$arch ./cmd/orchd; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
	      -o $(BIN)/orch-$$os-$$arch ./cmd/orch; \
	  done; \
	done
	@ls -lh $(BIN)/

.PHONY: run
run: build
	$(BIN)/orchd serve --config examples/orch.yaml --db /tmp/orch.journal --allow-fake-provider

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out
