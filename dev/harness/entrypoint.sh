#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# Start the virtual printer and a CUPS that points at it, then idle.
set -eu

# ippeveprinter 2.4 exits when it cannot reach a DNS-SD daemon, and "-r off"
# does not skip that, so the container runs dbus and avahi for it alone.
mkdir -p /run/dbus
rm -f /run/dbus/pid
dbus-daemon --system --fork
avahi-daemon --daemonize --no-drop-root --no-chroot
i=0
until [ -S /run/avahi-daemon/socket ]; do
    i=$((i + 1))
    [ "$i" -le 100 ] || { echo "avahi never opened its socket" >&2; exit 1; }
    sleep 0.1
done

printer-start &
printer=$!
cupsd -f &

# printer_alive says whether the printer started above still runs. A killed
# child that nobody has waited for stays a zombie, which kill -0 still finds.
printer_alive() {
    state=$(ps -o stat= -p "$printer" 2>/dev/null) || return 1
    case $state in Z* | "") return 1 ;; esac
}

i=0
until [ -S /run/cups/cups.sock ]; do
    i=$((i + 1))
    [ "$i" -le 100 ] || { echo "cupsd never opened its socket" >&2; exit 1; }
    sleep 0.1
done

# Retry: ippeveprinter may not answer yet when cupsd first asks for its attributes.
i=0
until lpadmin -p Virtual-MFC -E -v ipp://localhost:8631/ipp/print -m everywhere; do
    printer_alive || { echo "ippeveprinter died; could not add Virtual-MFC" >&2; exit 1; }
    i=$((i + 1))
    [ "$i" -le 30 ] || { echo "could not add Virtual-MFC" >&2; exit 1; }
    sleep 1
done

# CUPS learns the supply levels only from the first job it sends to a printer,
# so a fresh queue reports no markers. Print one page, wait until the supplies
# show up, then purge the job so the history starts empty.
echo "skra harness warm-up" | lp -d Virtual-MFC >/dev/null
i=0
until lpoptions -p Virtual-MFC | grep -q 'marker-names='; do
    printer_alive || { echo "ippeveprinter died; Virtual-MFC reported no supplies" >&2; exit 1; }
    i=$((i + 1))
    [ "$i" -le 60 ] || { echo "Virtual-MFC never reported its supplies" >&2; exit 1; }
    sleep 1
done
i=0
until [ -z "$(lpstat -o Virtual-MFC)" ]; do
    i=$((i + 1))
    [ "$i" -le 60 ] || { echo "the warm-up job never finished" >&2; exit 1; }
    sleep 0.5
done
cancel -a -x Virtual-MFC
echo "skra-harness ready"

# A shell loop, not a foreground "sleep infinity": a shell runs a trap only
# after its foreground command ends, so the TERM trap would wait for it. In
# the loop, "wait" returns the moment a signal arrives and the trap runs at
# once; the background sleep is left to die with the container.
trap 'exit 0' TERM INT
while :; do
    sleep 3600 &
    wait $! || true
done
