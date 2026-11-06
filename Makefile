.PHONY: all build test fmt vet cyclo ineffassign misspell staticcheck govulncheck lint check site hero

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

# peekm.dev: site/ plus the app fonts and the README screenshot.
site:
	mkdir -p site/fonts && cp theme/fonts/*.woff2 site/fonts/ && cp assets/hero.png site/

# README + peekm.dev screenshot: the demo session rendered by the real transcript handler (sitedemo_test.go).
hero:
	dir=$$(mktemp -d) && go test -count=1 -run '^TestSiteDemo$$' . -args -site-demo-out=$$dir/demo.html && node scripts/hero.mjs $$dir/demo.html assets/hero.png
