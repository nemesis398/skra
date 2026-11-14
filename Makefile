.PHONY: build test integration run harness-up harness-down printer-stop printer-start

build:
	go build -o skra ./cmd/skra

test:
	go test ./...

integration: harness-up
	go test -count=1 -tags integration ./...

harness-up:
	./dev/harness.sh up

harness-down:
	./dev/harness.sh down

printer-stop:
	./dev/harness.sh printer-stop

printer-start:
	./dev/harness.sh printer-start

run: build
	./skra
