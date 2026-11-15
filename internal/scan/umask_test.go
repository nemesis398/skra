// SPDX-License-Identifier: AGPL-3.0-or-later

package scan

import "syscall"

// umaskZero clears the umask and returns a function that puts it back.
func umaskZero() func() {
	old := syscall.Umask(0)
	return func() { syscall.Umask(old) }
}
