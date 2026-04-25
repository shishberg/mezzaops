.PHONY: check lint test build dev cli clean

# The goolm build tag selects the pure-Go Olm implementation used by the
# Matrix frontend's E2EE crypto helper. Without it, mautrix imports its libolm
# cgo wrapper, which requires the libolm C headers to be installed.
GO_TAGS := -tags goolm

check: lint test build

lint:
	golangci-lint run --build-tags goolm ./...

test:
	go test $(GO_TAGS) -race -count=1 ./...

build:
	go build $(GO_TAGS) -o bin/mezzaops .

dev:
	go run $(GO_TAGS) . --config config.dev.yaml

cli:
	go run $(GO_TAGS) . --config config.dev.yaml -i

clean:
	rm -rf bin/
