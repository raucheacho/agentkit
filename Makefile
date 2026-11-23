.PHONY: test build fmt vet

# Le module n'a besoin d'aucun cgo (SQLite via modernc.org/sqlite). GOWORK=off
# évite qu'un go.work parent (persibot) remplace ce module par une autre copie.
test:
	GOWORK=off go test -race ./...

vet:
	GOWORK=off go vet ./...

build:
	GOWORK=off go build ./...

fmt:
	gofmt -w .
