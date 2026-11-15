.PHONY: build test integration run dev install-user harness-up harness-down printer-stop printer-start

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

# install-user puts the binary in ~/.local/bin and the units in the user's
# systemd directory, and starts the socket. The service starts on the first
# request and exits when idle.
install-user: build
	install -D -m 0755 skra $(HOME)/.local/bin/skra
	install -D -m 0644 dist/skra.socket $(HOME)/.config/systemd/user/skra.socket
	install -D -m 0644 dist/skra.service $(HOME)/.config/systemd/user/skra.service
	systemctl --user daemon-reload
	systemctl --user enable --now skra.socket
	systemctl --user try-restart skra.service

run: build
	./skra

# dev runs skra against the test harness instead of the laptop's CUPS. The
# config and state live under .dev/, beside the harness's run/ directory.
dev: build harness-up
	@mkdir -p .dev/config/skra
	@printf '{"cups_socket": "%s/.dev/run/cups.sock", "user": "root", "state_dir": "%s/.dev/state"}\n' "$(CURDIR)" "$(CURDIR)" > .dev/config/skra/config.json
	XDG_CONFIG_HOME=$(CURDIR)/.dev/config ./skra
