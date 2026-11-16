// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// tokenBytes is how many random bytes a token holds.
const tokenBytes = 32

// NewToken returns a fresh token: 32 random bytes as unpadded base64url.
func NewToken() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("skra: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// LoadToken returns the token kept in dir/token, and makes it when there is
// none. The token outlives the process, so a page that was open while the
// service exited when idle can still act once the socket starts it again.
//
// A new token is written to a hidden temp file of mode 0600 in dir, synced,
// and then hard-linked to dir/token, so the token file never exists half
// written. A link fails when the name is taken: of two first starts at once,
// such as one by hand next to the socket's, the later one reads the token of
// the first. A file that is there but is not a regular file (a symlink
// included), has any permission bit for the group or for others, is longer
// than tokenFileMax, or does not hold a token of 32 bytes or more, with at
// most one line end after it, is refused, so the service does not start.
// Deleting the file makes a new token at the next start.
func LoadToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("token: create %s: %w", dir, err)
	}
	_, err := os.Lstat(path)
	if err == nil {
		return readToken(path)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("token: %w", err)
	}
	tok := NewToken()
	tmp, err := writeTokenTemp(dir, tok)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	err = os.Link(tmp, path)
	if errors.Is(err, fs.ErrExist) {
		return readToken(path) // another start linked its token first
	}
	if err != nil {
		return "", fmt.Errorf("token: link %s: %w; put state_dir on a file system with hard links", path, err)
	}
	return tok, nil
}

// writeTokenTemp writes tok to a new hidden temp file in dir and returns its
// path, with the data on disk. On an error it removes the file.
func writeTokenTemp(dir, tok string) (string, error) {
	// CreateTemp makes the file 0600 and never reuses a name.
	f, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return "", fmt.Errorf("token: create a temp file in %s: %w", dir, err)
	}
	_, werr := f.WriteString(tok)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("token: write %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

// tokenFileMax caps how much of the token file is read: a token is 43
// bytes, so a file longer than this holds something else.
const tokenFileMax = 1024

// readToken reads and checks a token file that exists. It opens the path
// once and checks the file it opened, so the path cannot be swapped between
// the check and the read. O_NOFOLLOW refuses a symlink, and O_NONBLOCK keeps
// a FIFO from waiting for a writer before it is refused.
func readToken(path string) (string, error) {
	bad := func(what string) (string, error) {
		return "", fmt.Errorf("token: %s %s; delete it and skrá makes a new token at the next start", path, what)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return bad("is not a regular file")
	}
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return bad("is not a regular file")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return bad(fmt.Sprintf("has mode %04o, so %s; want 0600", perm, openTo(perm)))
	}
	raw, err := io.ReadAll(io.LimitReader(f, tokenFileMax+1))
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if len(raw) > tokenFileMax {
		return bad(fmt.Sprintf("is too long for a token (over %d bytes)", tokenFileMax))
	}
	// One line end is what echo or an editor leaves; it is not part of the
	// token. The decoder skips CR and LF wherever they are, so any other is
	// refused here, or it would end up in the token and the header.
	tok := string(raw)
	if t, ok := strings.CutSuffix(tok, "\n"); ok {
		tok = strings.TrimSuffix(t, "\r")
	}
	if tok == "" {
		return bad("is empty")
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || strings.ContainsAny(tok, "\r\n") {
		return bad("is not a token")
	}
	if len(b) < tokenBytes {
		return bad(fmt.Sprintf("holds a token that is too short (%d bytes, want %d)", len(b), tokenBytes))
	}
	return tok, nil
}

// openTo says who besides the owner may do what with a file of mode perm,
// such as "the group can read it and others can read and write it".
func openTo(perm fs.FileMode) string {
	can := func(bits fs.FileMode) string {
		var what []string
		for _, b := range []struct {
			bit  fs.FileMode
			verb string
		}{{4, "read"}, {2, "write"}, {1, "execute"}} {
			if bits&b.bit != 0 {
				what = append(what, b.verb)
			}
		}
		if len(what) == 1 {
			return what[0] + " it"
		}
		return strings.Join(what[:len(what)-1], ", ") + " and " + what[len(what)-1] + " it"
	}
	group, others := perm>>3&7, perm&7
	switch {
	case group == others:
		return "the group and others can " + can(group)
	case others == 0:
		return "the group can " + can(group)
	case group == 0:
		return "others can " + can(others)
	}
	return "the group can " + can(group) + " and others can " + can(others)
}
