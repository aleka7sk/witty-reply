.PHONY: build test test-race vet fmt check run

build:
	go build ./cmd/witty-reply

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

check: fmt vet test-race build

run:
	go run ./cmd/witty-reply
