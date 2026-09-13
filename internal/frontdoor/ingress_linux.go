package frontdoor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"persea-terminal/internal/config"
	"persea-terminal/internal/unixcred"
)

type trustedConn struct {
	net.Conn
	uid     uint32
	once    sync.Once
	release func()
}

func (c *trustedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }

type ingressListener struct {
	*net.UnixListener
	expected uint32
	slots    chan struct{}
}

var ingressPeerUID = unixcred.PeerUID
var ingressVerify = unixcred.Verify
var ingressChmod = os.Chmod

func (l *ingressListener) Accept() (net.Conn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
		default:
			logIngressReason("conn_limit")
			_ = c.Close()
			continue
		}
		uid, e := ingressPeerUID(c)
		if e != nil {
			<-l.slots
			logIngress("peer_cred", uid)
			_ = c.Close()
			continue
		}
		if uid != l.expected {
			<-l.slots
			logIngress("peer_uid", uid)
			_ = c.Close()
			continue
		}
		if e = ingressVerify(c, l.expected); e != nil {
			<-l.slots
			logIngress("peer_cred", uid)
			_ = c.Close()
			continue
		}
		return &trustedConn{Conn: c, uid: uid, release: func() { <-l.slots }}, nil
	}
}

type boundSocket struct {
	info os.FileInfo
	uid  uint32
	gid  uint32
	mode os.FileMode
}

func captureBoundSocket(path string) (boundSocket, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return boundSocket{}, fmt.Errorf("bound socket lstat: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return boundSocket{}, errors.New("bound socket identity mismatch")
	}
	return boundSocket{info: info, uid: stat.Uid, gid: stat.Gid, mode: info.Mode().Perm()}, nil
}

func closeAndRemoveBound(ln *net.UnixListener, path string, bound boundSocket, allowedModes ...os.FileMode) error {
	var result error
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		result = errors.Join(result, fmt.Errorf("close bound socket: %w", err))
	}
	current, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.Join(result, errors.New("bound socket absent; refusing cleanup"))
		}
		return errors.Join(result, fmt.Errorf("bound socket cleanup lstat: %w", err))
	}
	stat, ok := current.Sys().(*syscall.Stat_t)
	modeOK := false
	for _, mode := range allowedModes {
		modeOK = modeOK || current.Mode().Perm() == mode
	}
	if !ok || current.Mode()&os.ModeSocket == 0 || current.Mode()&os.ModeSymlink != 0 || stat.Uid != bound.uid || stat.Gid != bound.gid || !modeOK || !os.SameFile(bound.info, current) {
		return errors.Join(result, errors.New("bound socket replaced or invalid; refusing cleanup"))
	}
	if err := os.Remove(path); err != nil {
		return errors.Join(result, fmt.Errorf("remove bound socket: %w", err))
	}
	return result
}

func listenIngress(cfg config.Ingress) (net.Listener, func() error, error) {
	parent := filepath.Dir(cfg.SocketPath)
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, nil, fmt.Errorf("socket parent: %w", err)
	}
	parentStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || parentStat.Uid != uint32(os.Geteuid()) {
		return nil, nil, errors.New("unsafe socket parent")
	}
	if strings.HasPrefix(cfg.SocketPath, "/run/") {
		for ancestor := filepath.Dir(parent); ; ancestor = filepath.Dir(ancestor) {
			item, e := os.Lstat(ancestor)
			if e != nil || !item.IsDir() || item.Mode()&os.ModeSymlink != 0 || item.Mode().Perm()&0022 != 0 {
				return nil, nil, errors.New("unsafe socket ancestor")
			}
			if ancestor == "/" {
				break
			}
		}
	}
	if old, e := os.Lstat(cfg.SocketPath); e == nil {
		oldStat, statOK := old.Sys().(*syscall.Stat_t)
		if !statOK || old.Mode()&os.ModeSocket == 0 || old.Mode().Perm() != 0600 || oldStat.Uid != uint32(os.Geteuid()) {
			return nil, nil, errors.New("socket path occupied")
		}
		if e = probeStale(cfg.SocketPath); e != nil {
			return nil, nil, e
		}
		current, e := os.Lstat(cfg.SocketPath)
		if e != nil || !os.SameFile(old, current) {
			return nil, nil, errors.New("stale socket replaced")
		}
		if e = os.Remove(cfg.SocketPath); e != nil {
			return nil, nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, nil, e
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.SocketPath, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	ln.SetUnlinkOnClose(false)
	initial, err := captureBoundSocket(cfg.SocketPath)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	if err = ingressChmod(cfg.SocketPath, 0600); err != nil {
		cleanupErr := closeAndRemoveBound(ln, cfg.SocketPath, initial, initial.mode, 0600)
		return nil, nil, errors.Join(fmt.Errorf("chmod bound socket: %w", err), cleanupErr)
	}
	bound, err := captureBoundSocket(cfg.SocketPath)
	if err != nil || bound.mode != 0600 || !os.SameFile(initial.info, bound.info) {
		cleanupErr := closeAndRemoveBound(ln, cfg.SocketPath, initial, initial.mode, 0600)
		return nil, nil, errors.Join(errors.New("bound socket state mismatch"), err, cleanupErr)
	}
	cleanup := func() error {
		return closeAndRemoveBound(ln, cfg.SocketPath, bound, 0600)
	}
	return &ingressListener{UnixListener: ln, expected: cfg.PeerUID, slots: make(chan struct{}, cfg.MaxConnections)}, cleanup, nil
}
func probeStale(path string) error {
	c, e := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if e == nil {
		_ = c.Close()
		return errors.New("socket already active")
	}
	if errors.Is(e, syscall.ECONNREFUSED) {
		return nil
	}
	return fmt.Errorf("unsafe stale socket: %w", e)
}
