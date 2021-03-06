/* SPDX-License-Identifier: GPL-2.0
 *
 * Copyright (C) 2017-2018 Jason A. Donenfeld <Jason@zx2c4.com>. All Rights Reserved.
 */

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path"
)

const (
	ipcErrorIO        = -int64(unix.EIO)
	ipcErrorProtocol  = -int64(unix.EPROTO)
	ipcErrorInvalid   = -int64(unix.EINVAL)
	ipcErrorPortInUse = -int64(unix.EADDRINUSE)
	socketDirectory   = "/var/run/wireguard"
	socketName        = "%s.sock"
)

type UAPIListener struct {
	listener net.Listener // unix socket listener
	connNew  chan net.Conn
	connErr  chan error
	kqueueFd int
	keventFd int
}

func (l *UAPIListener) Accept() (net.Conn, error) {
	for {
		select {
		case conn := <-l.connNew:
			return conn, nil

		case err := <-l.connErr:
			return nil, err
		}
	}
}

func (l *UAPIListener) Close() error {
	err1 := unix.Close(l.kqueueFd)
	err2 := unix.Close(l.keventFd)
	err3 := l.listener.Close()
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}

func (l *UAPIListener) Addr() net.Addr {
	return nil
}

func UAPIListen(name string, file *os.File) (net.Listener, error) {

	// wrap file in listener

	listener, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}

	uapi := &UAPIListener{
		listener: listener,
		connNew:  make(chan net.Conn, 1),
		connErr:  make(chan error, 1),
	}

	socketPath := path.Join(
		socketDirectory,
		fmt.Sprintf(socketName, name),
	)

	// watch for deletion of socket

	uapi.kqueueFd, err = unix.Kqueue()
	if err != nil {
		return nil, err
	}
	uapi.keventFd, err = unix.Open(socketDirectory, unix.O_EVTONLY, 0)
	if err != nil {
		unix.Close(uapi.kqueueFd)
		return nil, err
	}

	go func(l *UAPIListener) {
		event := unix.Kevent_t{
			Ident:  uint64(uapi.keventFd),
			Filter: unix.EVFILT_VNODE,
			Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
			Fflags: unix.NOTE_WRITE,
		}
		events := make([]unix.Kevent_t, 1)
		n := 1
		var kerr error
		for {
			// start with lstat to avoid race condition
			if _, err := os.Lstat(socketPath); os.IsNotExist(err) {
				l.connErr <- err
				return
			}
			if kerr != nil || n != 1 {
				return
			}
			n, kerr = unix.Kevent(uapi.kqueueFd, []unix.Kevent_t{event}, events, nil)
		}
	}(uapi)

	// watch for new connections

	go func(l *UAPIListener) {
		for {
			conn, err := l.listener.Accept()
			if err != nil {
				l.connErr <- err
				break
			}
			l.connNew <- conn
		}
	}(uapi)

	return uapi, nil
}

func UAPIOpen(name string) (*os.File, error) {

	// check if path exist

	err := os.MkdirAll(socketDirectory, 0600)
	if err != nil && !os.IsExist(err) {
		return nil, err
	}

	// open UNIX socket

	socketPath := path.Join(
		socketDirectory,
		fmt.Sprintf(socketName, name),
	)

	addr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, err
	}

	listener, err := func() (*net.UnixListener, error) {

		// initial connection attempt

		listener, err := net.ListenUnix("unix", addr)
		if err == nil {
			return listener, nil
		}

		// check if socket already active

		_, err = net.Dial("unix", socketPath)
		if err == nil {
			return nil, errors.New("unix socket in use")
		}

		// cleanup & attempt again

		err = os.Remove(socketPath)
		if err != nil {
			return nil, err
		}
		return net.ListenUnix("unix", addr)
	}()

	if err != nil {
		return nil, err
	}

	return listener.File()
}
