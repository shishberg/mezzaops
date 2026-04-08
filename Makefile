.PHONY: check lint test build dev clean

check: lint test build

lint:
	golangci-lint run ./...

test:
	go test -race -count=1 ./...

build:
	go build -o bin/mezzaops .

dev:
	go run . --config config.dev.yaml

clean:
	rm -rf bin/
