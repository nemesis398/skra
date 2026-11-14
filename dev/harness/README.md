# Test harness

A rootless podman container with its own CUPS and a virtual IPP Everywhere
printer. Integration tests talk to it through `.dev/run/cups.sock`, so nothing
touches the laptop's real CUPS.

    make harness-up       build and start (reuses a running container)
    make integration      harness-up, then go test -tags integration ./...
    make printer-stop     stop ippeveprinter inside the container
    make printer-start    start it again
    make harness-down     remove the container

The queue is `Virtual-MFC` at `ipp://localhost:8631/ipp/print`, published on
`127.0.0.1:8631`. Tests get a client from `internal/testenv`, which acts as user
`root` (the host user maps to root inside the rootless container, and CUPS
checks the PeerCred name against the socket peer) and skips when the socket is
missing.

## Markers

`ippeveprinter` does serve the marker attributes from `printer.attrs`: four
inks (Black 68, Cyan 48, Magenta 56, Yellow 12; low level 10). Three things
about it are not obvious:

- `-a` replaces the printer's default attributes instead of adding to them, so
  `printer.attrs` lists every attribute that `lpadmin -m everywhere` needs, not
  only the markers.
- `-a` cannot be combined with `-M` or `-m`: `ippeveprinter` 2.4 prints its
  usage and exits. Make and model are `ATTR` lines in `printer.attrs`.
- CUPS learns marker levels only from the first job it sends to a queue, so a
  fresh queue reports none. `entrypoint.sh` prints one warm-up page and purges
  it, so the container is "ready" with four markers and an empty job history.

`ippeveprinter` also exits without a DNS-SD daemon (`-r off` does not skip
that), which is why the image runs dbus and avahi.
