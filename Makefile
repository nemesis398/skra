.PHONY: build test integration run

build:
	go build -o skra ./cmd/skra

test:
	go test ./...

integration:
	go test -tags integration ./...

run: build
	./skra
