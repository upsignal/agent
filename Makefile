VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
PKG     := github.com/upsignal/agent/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build linux test vet fmt clean

build:
	$(BUILD) -o dist/upsignal-agent ./cmd/upsignal-agent

linux:
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/upsignal-agent-linux-amd64 ./cmd/upsignal-agent
	GOOS=linux GOARCH=arm64 $(BUILD) -o dist/upsignal-agent-linux-arm64 ./cmd/upsignal-agent
	cd dist && shasum -a 256 upsignal-agent-linux-* > SHA256SUMS

test:
	go test -race ./...

vet:
	go vet ./...
	GOOS=linux go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

clean:
	rm -rf dist
