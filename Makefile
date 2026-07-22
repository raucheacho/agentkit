.PHONY: test build fmt

# mattn/go-sqlite3 (session/store) requires cgo.
test:
	CGO_ENABLED=1 go test -race ./...

build:
	CGO_ENABLED=1 go build ./...

fmt:
	go fmt ./...
