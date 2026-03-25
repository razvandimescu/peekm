.PHONY: all build test fmt vet cyclo ineffassign misspell staticcheck vulncheck lint check

all: check

build:
	go build ./...

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

vulncheck:
	govulncheck ./...

lint: fmt vet cyclo ineffassign misspell staticcheck vulncheck

check: build test lint
