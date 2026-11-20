package processgroup

import "golang.org/x/sys/unix"

// Observe exit without reaping the leader. On observation failure the caller
// closes the group before reaping it, so it never retains an unowned group ID.
func observeExit(pid int) {
	var status unix.Siginfo
	for {
		if err := unix.Waitid(unix.P_PID, pid, &status, unix.WEXITED|unix.WNOWAIT, nil); err != unix.EINTR {
			return
		}
	}
}
