# skrá

[![License: AGPL-3.0-or-later](https://img.shields.io/badge/license-AGPL--3.0--or--later-cba6f7?style=flat-square&labelColor=313244)](LICENSE)

A small web service that replaces the CUPS web interface on a Linux laptop. It
shows your printers with live ink levels, prints a file you drop on the page,
manages the queue, adds and configures printers, and reads the CUPS log. It
runs as your own user, listens on `127.0.0.1:7631` only, and sleeps when
nobody uses it: systemd starts it on the first request, and it exits after five
idle minutes.

![The overview: printers with ink, printing a file, the queue and the last prints](docs/overview.png)

![The printers tab with a queue's settings open](docs/printers.png)

## 🖨️ What it does

- **Overview:** each printer with its state and ink levels, a test page button,
  and printing a PDF, JPEG, PNG or text file by dropping it on the page. The
  page updates itself over Server-Sent Events while it is visible.
- **Queue:** hold, release, move to another printer and cancel your jobs.
  Canceling a job that is already printing asks first, and so does anything
  that destroys more than one job.
- **Printers:** find printers on the network and over USB, add one as a
  driverless queue or with a driver CUPS has, and remove one. A queue's
  settings cover description, location, address, paused or running, accepting
  jobs, sharing, default sides, color, quality and paper, and what to do when
  printing fails. Maintenance commands the printer offers, such as cleaning
  the print heads, sit beside them.
- **CUPS log:** the last lines of the `cups.service` journal, with a switch for
  debug logging.
- **History:** the finished jobs CUPS keeps, each with a reprint button.

Every action is recorded with the user who did it, in a SQLite file under the
state directory.

## 📦 Install

You need Go, a running CUPS 2.4 and a systemd user session.

```sh
git clone https://github.com/nemesis398/skra.git
cd skra
make install-user
```

That builds the binary, copies it to `~/.local/bin/skra`, copies the two units
from `dist/` to `~/.config/systemd/user/`, and runs
`systemctl --user enable --now skra.socket`. Then open
<http://127.0.0.1:7631/>.

Only the socket is started. The service starts when the first request arrives,
exits after `idle_exit` without a request or an open page, and starts again
the next time you open the page. `make install-user` restarts a running
service so that it picks up a new binary.

To remove it, run `systemctl --user disable --now skra.socket skra.service`
and delete the three files.

## 🔑 CUPS rights

skra talks to CUPS over the local socket `/run/cups/cups.sock`, never over TCP.
Every request carries `Authorization: PeerCred <user>` and
`requesting-user-name <user>`. CUPS reads your user from the connection and
grants admin operations to members of `wheel` (or `lpadmin` or `sys` on other
distributions) without a password. skra asks for no password, holds none and
stores none, and the YubiKey step in a CUPS web login never comes into play.

The service adds no new rights: any program running as you can send the same
requests with `lpadmin` today. It adds a new door, and the door is guarded:

- It listens on `127.0.0.1` or `localhost` and refuses any other host in
  `listen`.
- Every request needs a `Host` of `127.0.0.1` or `localhost` with the port
  skra listens on (`7631` unless you change it), which stops DNS rebinding.
- Every request that changes something also needs the token in the
  `X-Skra-Token` header, and an `Origin` that is absent or the page's own.
  Anything else gets `403`. The token is 32 random bytes in `token` in the
  state directory, mode `0600`, so a page left open across an idle exit
  keeps working. skra refuses to start when that file is empty, short or
  readable by others; delete it, and the next start makes a new one.
- No other page may frame skrá.

## ⚙️ Configuration

No file is needed. To change a default, write `~/.config/skra/config.json`. An
unknown key is an error.

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:7631` | `127.0.0.1` or `localhost`, on any port. Under socket activation the socket unit decides, and the `Host` check follows its port. |
| `cups_socket` | `/run/cups/cups.sock` | The CUPS socket. |
| `user` | `$USER` | The name sent to CUPS and recorded with every action. |
| `state_dir` | `~/.local/state/skra` | Where the action log and the token live. |
| `idle_exit` | `5m` | How long the service waits with no request before it exits. Ignored when it was not started by its socket. |
| `declared_queues` | none | Queue names that your system configuration owns. They show as read-only and cannot be removed or re-addressed here. |
| `cupsd_conf_managed` | `false` | Set it when `cupsd.conf` is managed elsewhere, and the debug switch turns off. |

## 🧪 The harness

The tests never touch your real CUPS. `make harness-up` starts a rootless
podman container with its own CUPS and a virtual IPP Everywhere printer, and
its socket appears at `.dev/run/cups.sock`.

```sh
make test          unit tests, no container needed
make integration   starts the harness, then runs every test against it
make dev           skra on 127.0.0.1:7631 against the harness
make harness-down  remove the container
```

`make dev` is how to look at the interface without a printer of your own. The
screenshots above come from it. `dev/harness/README.md` has the details.

## ⚡ Budgets

Each one is a test in `internal/web/budget_test.go`, run by `make integration`.

| Path | Budget | Measured against the harness |
| --- | --- | --- |
| Any page, server time | p95 under 50 ms | under 1 ms |
| A CUPS action (hold, release) | p95 under 150 ms | about 1 ms |
| Page, style and script together | under 200 KB | 38 KB |
| Resident memory, idle | under 50 MB | about 22 MB |
| Timers and polling with no page open | none | the idle timer only |

While a page is open, skra asks CUPS every 2 seconds and the printers
themselves every 5 seconds, and pushes only what changed. When the last page
closes or hides, all of that stops. The one timer left is the idle timer that
ends the process.

## ⚠️ Known limits

- **Ink levels are the printer's own, when it answers.** When it does not,
  skra shows CUPS's copy, which CUPS reads only while it prints, and says when
  that was.
- **History is what CUPS keeps.** skra lists the completed jobs CUPS still
  remembers, up to 200. It keeps no archive of its own.
- **The CUPS log needs journal access.** Reading it runs `journalctl`, which
  your user may not be allowed to do, and the section then says so.
- **Declarative systems.** On NixOS or another system that declares queues in
  its configuration, those queues come back after a rebuild. List them in
  `declared_queues` so skra shows them read-only.
- **Print and CUPS only.** Scanning is not part of this version.

## 📄 License

The code is licensed under [AGPL-3.0-or-later](LICENSE).
