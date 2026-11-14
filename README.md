# cups-ui

A modern web interface for CUPS, the Linux print server. It is one HTML file that
CUPS serves itself, so it adds no daemon, no port and no build step. The colors
are Catppuccin Mocha.

![The overview: printers with ink, printing a file, the queue and the last prints](docs/overview.png)

![The printers tab with a queue's settings open](docs/printers.png)

## 🖨️ What it does

- **Overview:** each printer with its state and ink levels, a test page button,
  and printing a PDF, JPEG, PNG or text file by dropping it on the page.
- **Queue:** hold, release and cancel your jobs. Cancelling a job that is already
  printing asks first.
- **Printers:** find printers on the network and over USB, add one as a
  driverless queue or with a driver CUPS has, and remove one. A queue's
  settings cover description, location, address, paused or running, accepting
  jobs, sharing, and default sides, color, quality and paper.
- **History:** the jobs CUPS still remembers.

The page refreshes every 2 seconds while it is visible and stops when it is
hidden. Every request is a few milliseconds of IPP against the local CUPS.

## 📦 Install

You need CUPS 2.4 and a clone of this repository.

```sh
git clone https://github.com/OWNER/cups-ui.git
cd cups-ui
sudo ./install.sh
```

Then open <http://localhost:631/print/>.

The installer copies `index.html` to `/usr/share/cups/www/print/` and writes
your user name into it. CUPS needs that name: without it, CUPS hides job names
and refuses to let you cancel your own jobs. To install for another user, or
into a CUPS whose DocumentRoot is somewhere else:

```sh
sudo ./install.sh alex /usr/local/share/cups/www
```

To remove it:

```sh
sudo ./install.sh --remove
```

## 🔑 The CUPS login

Your own jobs need no login. Everything that changes a printer does: adding,
removing, settings, pausing, and searching for printers. CUPS answers those
requests with a login prompt from the browser, and the browser keeps the
login for the session. On Fedora any member of `wheel` may sign in; other
distributions use `lpadmin` or `sys`.

The CUPS tab leads to CUPS's own pages for anything this page does not cover,
such as classes and server settings.

## 🧭 How it works

CUPS serves static files from its DocumentRoot, so the page and CUPS share an
origin. The page speaks IPP over plain `fetch` POST requests to
`localhost:631`, encodes and decodes the binary IPP messages itself in about
70 lines, and needs no proxy and no backend.

## ⚠️ Known limits

- **Ink levels are CUPS's copy.** CUPS reads them only while it prints, so the
  page shows when they were last read.
- **Ink names.** Some driverless printers send levels without names. The page
  then labels the cartridges `#1` to `#4`.
- **Away printers.** CUPS notices that a network printer is gone only when the
  printer reports `offline`, so a printer that is simply switched off still
  shows as ready.
- **Declarative systems.** On NixOS, queues declared in the configuration come
  back after a rebuild, whatever you remove here.

## 📄 License

Not chosen yet.
