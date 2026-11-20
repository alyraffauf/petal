package processgroup

import (
	"syscall"
)

// Observe exit without reaping the leader. A completed exit may also cause
// registration to fail with ESRCH if the process is already a zombie.
func observeExit(pid int) {
	syscall.ForkLock.RLock()
	queue, err := syscall.Kqueue()
	if err == nil {
		syscall.CloseOnExec(queue)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return
	}
	defer syscall.Close(queue)
	change := []syscall.Kevent_t{{
		Ident: uint64(pid), Filter: syscall.EVFILT_PROC,
		Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT,
	}}
	events := make([]syscall.Kevent_t, 1)
	for {
		if _, err = syscall.Kevent(queue, change, events, nil); err != syscall.EINTR {
			return
		}
		// An interrupted kevent may already have installed the change.
		// Re-registering it also handles interruption before installation.
	}
}
