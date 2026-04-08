.PHONY: check lint test build dev cli clean

check: lint test build

lint:
	golangci-lint run ./...

test:
	go test -race -count=1 ./internal/...

build:
	go build -o bin/mezzaops .

dev:
	go run . --config config.dev.yaml

cli:
	go run . --config config.dev.yaml -i

clean:
	rm -rf bin/
