.PHONY: test build

test:
	go test ./...

build:
	go build -o ask-jev-$(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m) ./cmd/ask-jev
