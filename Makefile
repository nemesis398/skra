.PHONY: build test fmt-check integration run dev install-user harness-up harness-down printer-stop printer-start

build:
	go build -o skra ./cmd/skra

test:
	go test ./...

# fmt-check fails and lists the tracked Go files that gofmt would change.
# It names them because "gofmt -l ." also walks .dev, where the harness's
# certs directory is unreadable to the host user.
fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go')) && test -z "$$out" || { echo "gofmt would change:" >&2; echo "$$out" >&2; exit 1; }

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

# dev runs skra against the test harness instead of the laptop's CUPS, with
# the fake eSCL scanner on 127.0.0.1:8632 beside it, stopped when skra exits.
# The config, state and scans live under .dev/, beside the harness's run/
# directory. The config lists the fake scanner, so it shows without avahi.
dev: build harness-up
	@mkdir -p .dev/config/skra
	@printf '{"cups_socket": "%s/.dev/run/cups.sock", "user": "root", "state_dir": "%s/.dev/state", "scanners": [{"name": "skrá fake scanner", "url": "http://127.0.0.1:8632/eSCL"}], "scan_folder": "%s/.dev/scans"}\n' "$(CURDIR)" "$(CURDIR)" "$(CURDIR)" > .dev/config/skra/config.json
	go build -o .dev/fakescanner ./cmd/fakescanner
	.dev/fakescanner & scanner=$$!; \
	trap 'kill $$scanner' EXIT; \
	XDG_CONFIG_HOME=$(CURDIR)/.dev/config ./skra
