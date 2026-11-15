// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testenv gives integration tests a CUPS client for the harness
// container, so no test ever touches the laptop's own CUPS, and any test an
// address of this machine.
package testenv

import (
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nemesis398/skra/internal/cups"
)

// Container is the name of the harness container. dev/harness.sh holds the
// only other copy, as "name"; the two must agree.
const Container = "skra-harness"

// user is root because the host user maps to root inside the rootless
// container, and CUPS checks the PeerCred name against the socket peer.
const user = "root"

// The harness container has two users besides root, both with the password
// Password: Admin is in wheel, which CUPS's SystemGroup lists, and NonAdmin
// is not. A client acting as either sends a PeerCred that CUPS does not
// accept, because the socket peer is root; it gets in only where the harness
// asks for no login, or with Basic and the password.
const (
	Admin    = "skra-admin"
	NonAdmin = "skra-user"
	Password = "hunter2-skra"
)

// Socket returns the harness socket path under the repository root.
func Socket(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".dev", "run", "cups.sock")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// Client returns a client for the harness CUPS. It skips the test when the
// harness socket does not exist; "make integration" starts the harness. The
// test holds the restart lock shared until it ends, so a test that restarts
// the scheduler (ExclusiveClient) waits for it and never runs beside it.
func Client(tb testing.TB) *cups.Client {
	tb.Helper()
	return client(tb, syscall.LOCK_SH)
}

// ExclusiveClient is Client for a test that makes the scheduler restart, as a
// change of cupsd.conf does: for a moment cupsd closes and opens its socket
// again, and a request in that moment fails. It holds the restart lock alone
// until the test ends, so no other test of any package is using the harness
// meanwhile, and a test that calls it must not call Client as well: its own
// shared lock would block it. "go test ./..." runs the packages at the same
// time, in separate processes.
func ExclusiveClient(tb testing.TB) *cups.Client {
	tb.Helper()
	return client(tb, syscall.LOCK_EX)
}

// ClientAs returns a client for the harness CUPS that acts as name, one of
// Admin and NonAdmin. It takes no lock, so the test calls ExclusiveClient
// first and holds the harness alone.
func ClientAs(tb testing.TB, name string) *cups.Client {
	tb.Helper()
	return cups.New(Socket(tb), name)
}

// requireSocket skips the test when sock does not exist and fails it on any
// other Stat error, such as a permission problem in .dev/run: a harness that
// is there but unreadable is a fault, not an absent harness.
func requireSocket(tb testing.TB, sock string) {
	tb.Helper()
	_, err := os.Stat(sock)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		tb.Skipf("harness is not running (%s: %v); run \"make harness-up\"", sock, err)
	default:
		tb.Fatalf("harness socket %s: %v", sock, err)
	}
}

// client skips the test when the harness socket does not exist, takes the
// restart lock as how says, and returns a client for the harness CUPS.
func client(tb testing.TB, how int) *cups.Client {
	tb.Helper()
	sock := Socket(tb)
	requireSocket(tb, sock)
	lock(tb, "cupsd.lock", how)
	return cups.New(sock, user)
}

// LockDefault holds a lock across processes until the test ends. The default
// queue is one setting of the one harness CUPS, and "go test ./..." runs the
// packages at the same time, so a test that changes the default, or reads
// what it was, takes the lock first. Otherwise one package restores the
// other's scratch queue as "the default before".
func LockDefault(tb testing.TB) {
	tb.Helper()
	lock(tb, "default.lock", syscall.LOCK_EX)
}

// lock takes a flock on a file in .dev, shared or exclusive, and holds it
// until the test ends.
func lock(tb testing.TB, name string, how int) {
	tb.Helper()
	path := filepath.Join(filepath.Dir(filepath.Dir(Socket(tb))), name) // .dev/NAME
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		tb.Fatal(err)
	}
	tb.Cleanup(func() { f.Close() }) // closing releases the lock
}

// InterfaceAddr returns an address of one of this machine's interfaces that is
// not loopback or link-local, or skips the test.
func InterfaceAddr(tb testing.TB) netip.Addr {
	tb.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		tb.Skip(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				return ip.Unmap()
			}
		}
	}
	tb.Skip("no interface address besides loopback")
	return netip.Addr{}
}

// Command returns a podman exec of args inside the harness container, not
// yet run. A cleanup that must not fail the test calls Run on it and drops the
// error.
func Command(args ...string) *exec.Cmd {
	return exec.Command("podman", append([]string{"exec", Container}, args...)...)
}

// Exec runs args inside the harness container and fails the test when the
// command does.
func Exec(tb testing.TB, args ...string) {
	tb.Helper()
	out, err := Command(args...).CombinedOutput()
	if err != nil {
		tb.Fatalf("podman exec %v: %v\n%s", args, err, out)
	}
}

// ScratchQueue makes a driverless queue in the harness container and removes
// it when the test ends. Tests that reject, pause, empty or remove a queue use
// one of their own, so a test of another package that prints on Virtual-MFC
// at the same moment is not hit; the names differ per package for the same
// reason.
func ScratchQueue(tb testing.TB, name string) {
	tb.Helper()
	Exec(tb, "lpadmin", "-p", name, "-E", "-v", "ipp://localhost:8631/ipp/print", "-m", "everywhere")
	tb.Cleanup(func() { Command("lpadmin", "-x", name).Run() }) // gone already is fine
}
