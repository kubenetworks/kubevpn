//go:build windows

package dns

import (
	"golang.org/x/sys/windows"
)

// withHostsFileLock serializes hosts-file writes across concurrent kubevpn instances using a
// global named mutex (the Windows counterpart of the Unix flock). If the mutex cannot be created
// or acquired, fn runs without the lock as a fallback (single-instance usage stays correct),
// matching the Unix behavior.
func withHostsFileLock(fn func() error) error {
	name, err := windows.UTF16PtrFromString(`Global\kubevpn-hosts-lock`)
	if err != nil {
		return fn()
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return fn()
	}
	defer windows.CloseHandle(h)

	// INFINITE matches flock(LOCK_EX)'s blocking semantics. If the previous owner's process died
	// without releasing, the OS abandons the mutex and the next waiter gets WAIT_ABANDONED (which
	// still transfers ownership), so this cannot deadlock on a crashed holder.
	s, err := windows.WaitForSingleObject(h, windows.INFINITE)
	if err != nil || (s != windows.WAIT_OBJECT_0 && s != uint32(windows.WAIT_ABANDONED)) {
		return fn()
	}
	defer windows.ReleaseMutex(h)

	return fn()
}
