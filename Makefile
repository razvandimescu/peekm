.PHONY: all build test fmt vet cyclo ineffassign misspell staticcheck lint check

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

lint: fmt vet cyclo ineffassign misspell staticcheck

check: build test lint
