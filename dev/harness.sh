#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Bring the test harness up (idempotent), take it down, or stop and start the
# virtual printer inside it. The harness is a rootless podman container with
# its own CUPS; its socket is ./.dev/run/cups.sock. It never touches the
# laptop's CUPS.
set -eu

name=skra-harness
image=localhost/skra-harness
root=$(cd "$(dirname "$0")/.." && pwd)
run="$root/.dev/run"

case "${1:-}" in
up)
    if [ "$(podman inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)" = true ] && [ -S "$run/cups.sock" ]; then
        echo "$name is already running"
        exit 0
    fi
    podman rm -f "$name" >/dev/null 2>&1 || true
    mkdir -p "$run"
    podman build -q -t "$image" "$root/dev/harness" >/dev/null
    podman run -d --name "$name" -p 127.0.0.1:8631:8631 -v "$run:/run/cups:Z" "$image" >/dev/null
    i=0
    until podman logs "$name" 2>&1 | command grep -q 'skra-harness ready'; do
        i=$((i + 1))
        if [ "$i" -gt 120 ] || [ "$(podman inspect -f '{{.State.Running}}' "$name")" != true ]; then
            podman logs "$name" >&2
            echo "$name did not become ready" >&2
            exit 1
        fi
        sleep 0.5
    done
    echo "$name is ready"
    ;;
down)
    podman rm -f "$name" >/dev/null 2>&1 || true
    ;;
printer-stop)
    podman exec "$name" pkill -x ippeveprinter || true
    ;;
printer-start)
    podman exec -d "$name" printer-start
    ;;
*)
    echo "usage: $0 up|down|printer-stop|printer-start" >&2
    exit 2
    ;;
esac
