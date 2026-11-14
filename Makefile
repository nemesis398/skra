.PHONY: build test integration run dev harness-up harness-down printer-stop printer-start

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

# dev runs skra against the test harness instead of the laptop's CUPS. The
# config and state live under .dev/, beside the harness's run/ directory.
dev: build harness-up
	@mkdir -p .dev/config/skra
	@printf '{"cups_socket": "%s/.dev/run/cups.sock", "user": "root", "state_dir": "%s/.dev/state"}\n' "$(CURDIR)" "$(CURDIR)" > .dev/config/skra/config.json
	XDG_CONFIG_HOME=$(CURDIR)/.dev/config ./skra
