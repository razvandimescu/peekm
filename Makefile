.PHONY: all build test fmt vet cyclo ineffassign misspell staticcheck govulncheck lint check site-demo

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

govulncheck:
	govulncheck ./...

lint: fmt vet cyclo ineffassign misspell staticcheck govulncheck

check: build test lint

# peekm.dev demo: rendered by the real transcript handler (sitedemo_test.go), plus the app fonts.
site-demo:
	go test -count=1 -run '^TestSiteDemo$$' . -args -site-demo-out=site/demo.html
	mkdir -p site/fonts && cp theme/fonts/*.woff2 site/fonts/
