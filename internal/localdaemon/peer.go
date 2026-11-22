//go:build linux || darwin

package localdaemon

import (
	"fmt"
	"net"
	"os"
)

func checkPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var uid uint32
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		uid, credentialErr = peerUID(int(fd))
	}); err != nil {
		return err
	}
	if credentialErr != nil {
		return fmt.Errorf("read daemon socket peer credentials: %w", credentialErr)
	}
	if uid != uint32(os.Geteuid()) {
		return fmt.Errorf("daemon socket peer UID %d differs from effective UID %d", uid, os.Geteuid())
	}
	return nil
}
