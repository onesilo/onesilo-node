BINARY  := onesilo-node
PKG     := github.com/onesilo/onesilo-node
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)

.PHONY: build test vet fmt fmt-check lint clean verify-builds image image-verify

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

lint: vet fmt-check

clean:
	rm -rf bin

# Build every supported platform and prove each is reproducible. Publishes
# nothing — the same check CI runs as a gate on a release tag.
verify-builds:
	./scripts/verify-builds.sh

# Build the Docker image (reproducible flags, single build).
image:
	./scripts/build-image.sh --single

# Build twice and assert the image IDs are identical.
image-verify:
	./scripts/build-image.sh
