.PHONY: all build test fmt vet cyclo ineffassign misspell staticcheck govulncheck lint check

all: check

COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -X main.commit=$(COMMIT) -X main.date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" ./...

test:
	go test -race ./...

fmt:
	@output=$$(gofmt -s -d .); \
	if [ -n "$$output" ]; then echo "$$output"; exit 1; fi

vet:
	go vet ./...

cyclo:
	gocyclo -over 15 .

ineffassign:
	ineffassign ./...

misspell:
	misspell -error .

staticcheck:
	staticcheck ./...

govulncheck:
	govulncheck ./...

lint: fmt vet cyclo ineffassign misspell staticcheck govulncheck

check: build test lint
