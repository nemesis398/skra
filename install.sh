#!/usr/bin/env bash
# Install the page into CUPS's DocumentRoot, so CUPS serves it at
# http://localhost:631/print/ and the page talks IPP to CUPS from the same origin.
#
#   sudo ./install.sh [user] [document-root]
#   sudo ./install.sh --remove [document-root]
#
# user           whose jobs the page names (default: the user who ran sudo)
# document-root  CUPS's DocumentRoot (default: /usr/share/cups/www, Fedora's)
set -euo pipefail

here=$(cd -- "$(dirname -- "$0")" && pwd)

if [[ ${1:-} == --remove ]]; then
  root=${2:-/usr/share/cups/www}
  rm -f -- "$root/print/index.html"
  rmdir -- "$root/print" 2>/dev/null || true
  echo "removed $root/print"
  exit 0
fi

user=${1:-${SUDO_USER:-}}
root=${2:-/usr/share/cups/www}

if [[ -z $user ]]; then
  echo "name the user whose jobs the page shows: sudo $0 <user>" >&2
  exit 2
fi
if [[ ! $user =~ ^[a-z_][a-z0-9_-]*$ ]]; then
  echo "not a user name: $user" >&2
  exit 2
fi
if [[ ! -f $root/cups.css ]]; then
  echo "$root does not look like CUPS's DocumentRoot (no cups.css)" >&2
  exit 2
fi

install -d -m 0755 -- "$root/print"
sed "s/__CUPS_UI_USER__/$user/" -- "$here/index.html" > "$root/print/index.html.new"
chmod 0644 -- "$root/print/index.html.new"
mv -- "$root/print/index.html.new" "$root/print/index.html"
command -v restorecon >/dev/null && restorecon -R -- "$root/print" || true
echo "installed: http://localhost:631/print/ (jobs of $user)"
