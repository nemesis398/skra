// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testenv gives integration tests a CUPS client for the harness
// container, so no test ever touches the laptop's own CUPS, and any test an
// address of this machine.
package testenv

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nemesis398/skra/internal/cups"
)

// user is root because the host user maps to root inside the rootless
// container, and CUPS checks the PeerCred name against the socket peer.
const user = "root"

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
// harness socket does not exist; "make integration" starts the harness.
func Client(tb testing.TB) *cups.Client {
	tb.Helper()
	sock := Socket(tb)
	if _, err := os.Stat(sock); err != nil {
		tb.Skipf("harness is not running (%s: %v); run \"make harness-up\"", sock, err)
	}
	return cups.New(sock, user)
}

// LockDefault holds a lock across processes until the test ends. The default
// queue is one setting of the one harness CUPS, and "go test ./..." runs the
// packages at the same time, so a test that changes the default, or reads
// what it was, takes the lock first. Otherwise one package restores the
// other's scratch queue as "the default before".
func LockDefault(tb testing.TB) {
	tb.Helper()
	path := filepath.Join(filepath.Dir(filepath.Dir(Socket(tb))), "default.lock") // .dev/default.lock
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
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

// ScratchQueue makes a driverless queue in the harness container and removes
// it when the test ends. Tests that reject, pause, empty or remove a queue use
// one of their own, so a test of another package that prints on Virtual-MFC
// at the same moment is not hit; the names differ per package for the same
// reason.
func ScratchQueue(tb testing.TB, name string) {
	tb.Helper()
	out, err := exec.Command("podman", "exec", "skra-harness", "lpadmin", "-p", name, "-E", "-v",
		"ipp://localhost:8631/ipp/print", "-m", "everywhere").CombinedOutput()
	if err != nil {
		tb.Fatalf("lpadmin -p %s: %v\n%s", name, err, out)
	}
	tb.Cleanup(func() { exec.Command("podman", "exec", "skra-harness", "lpadmin", "-x", name).Run() }) // gone already is fine
}
