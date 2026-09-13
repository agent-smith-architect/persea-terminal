package unixcred

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// PeerUID returns the kernel credential atomically bound to a connected AF_UNIX socket.
func PeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("peer credential requires a Unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credential control: %w", err)
	}
	var cred *syscall.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("peer credential control: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("peer credential read: %w", sockErr)
	}
	if cred == nil {
		return 0, errors.New("peer credential unavailable")
	}
	return cred.Uid, nil
}

func Verify(conn net.Conn, expected uint32) error {
	actual, err := PeerUID(conn)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("peer uid mismatch")
	}
	return nil
}
