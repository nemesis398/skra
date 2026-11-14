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
sleep 1

printer-start &
cupsd -f &

i=0
until [ -S /run/cups/cups.sock ]; do
    i=$((i + 1))
    [ "$i" -le 100 ] || { echo "cupsd never opened its socket" >&2; exit 1; }
    sleep 0.1
done

# Retry: ippeveprinter may not answer yet when cupsd first asks for its attributes.
i=0
until lpadmin -p Virtual-MFC -E -v ipp://localhost:8631/ipp/print -m everywhere; do
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
    i=$((i + 1))
    [ "$i" -le 60 ] || { echo "Virtual-MFC never reported its supplies" >&2; exit 1; }
    sleep 1
done
until [ -z "$(lpstat -o Virtual-MFC)" ]; do sleep 0.5; done
cancel -a -x Virtual-MFC
echo "skra-harness ready"

# A shell loop, not "sleep infinity": wait reaps the printer when it is stopped.
trap 'exit 0' TERM INT
while :; do
    sleep 3600 &
    wait $! || true
done
