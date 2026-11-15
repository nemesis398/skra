#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Bring the test harness up (idempotent), take it down, or stop and start the
# virtual printer inside it. The harness is a rootless podman container with
# its own CUPS; its socket is ./.dev/run/cups.sock. It never touches the
# laptop's CUPS.
set -eu

# The container name. The Go tests hold the only other copy, as
# testenv.Container in internal/testenv/testenv.go; the two must agree.
name=skra-harness
image=localhost/skra-harness
root=$(cd "$(dirname "$0")/.." && pwd -P)
run="$root/.dev/run"

case "${1:-}" in
up)
    # Reuse the container only if it runs, is mounted on THIS checkout's run
    # directory (another worktree may own the name and port 8631), and its
    # CUPS answers. A socket file left in $run does not count.
    mount=$(podman inspect -f '{{range .Mounts}}{{if eq .Destination "/run/cups"}}{{.Source}}{{end}}{{end}}' "$name" 2>/dev/null || true)
    if [ "$(podman inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)" = true ] &&
        [ "$mount" = "$run" ] && [ -S "$run/cups.sock" ] &&
        [ "$(podman exec "$name" lpstat -r 2>/dev/null)" = "scheduler is running" ]; then
        echo "$name is already running"
        exit 0
    fi
    podman rm -f "$name" >/dev/null 2>&1 || true
    mkdir -p "$run"
    rm -f "$run/cups.sock"
    podman build -q -t "$image" "$root/dev/harness" >/dev/null
    podman run -d --name "$name" -p 127.0.0.1:8631:8631 -v "$run:/run/cups:Z" "$image" >/dev/null
    # entrypoint.sh gives up after at most 10 s for avahi, 10 s for cupsd,
    # 30 tries of lpadmin, 60 s for the supplies and 30 s for the warm-up job,
    # about 140 s plus the time each command itself takes. 400 half seconds is
    # 200 s, so its own message, not this one, reports a stuck step.
    i=0
    until podman logs "$name" 2>&1 | command grep -q 'skra-harness ready'; do
        i=$((i + 1))
        if [ "$i" -gt 400 ] || [ "$(podman inspect -f '{{.State.Running}}' "$name")" != true ]; then
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
    # rm -f kills cupsd with SIGKILL, so it never removes its socket, and the
    # bind-mounted run directory keeps it: tests would find a socket that
    # nothing answers on.
    rm -f "$run/cups.sock"
    ;;
printer-stop)
    # pkill exits 1 when no process matched, which is fine; 125 and 126 mean
    # podman could not run it (the container is not running).
    podman exec "$name" pkill -x ippeveprinter || [ $? -eq 1 ]
    ;;
printer-start)
    rc=0
    podman exec "$name" pgrep -x ippeveprinter >/dev/null || rc=$?
    case $rc in
    0)
        echo "ippeveprinter is already running"
        exit 0
        ;;
    1) ;;
    *)
        echo "$name is not running; run \"$0 up\"" >&2
        exit 1
        ;;
    esac
    podman exec -d "$name" printer-start
    # exec -d returns before the printer listens: wait, for at most 10 s.
    i=0
    # lpstat -r exits 0 either way; it prints whether the server answered.
    until [ "$(podman exec "$name" lpstat -h localhost:8631 -r 2>/dev/null)" = "scheduler is running" ]; do
        i=$((i + 1))
        [ "$i" -le 100 ] || { echo "ippeveprinter did not answer on port 8631" >&2; exit 1; }
        sleep 0.1
    done
    echo "ippeveprinter is ready"
    ;;
*)
    echo "usage: $0 up|down|printer-stop|printer-start" >&2
    exit 2
    ;;
esac
