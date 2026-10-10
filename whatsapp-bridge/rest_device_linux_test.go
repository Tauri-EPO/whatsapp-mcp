package main

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRESTSocketDeviceBinding(t *testing.T) {
	for _, split := range []bool{false, true} {
		listener, bound, err := listenREST("127.0.0.1", 0, split)
		if err != nil {
			t.Fatal(err)
		}
		connection, err := listener.(*net.TCPListener).SyscallConn()
		if err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		var device string
		var optionErr error
		err = connection.Control(func(fd uintptr) {
			device, optionErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		})
		_ = listener.Close()
		if err != nil || optionErr != nil {
			t.Fatalf("read real socket option: %v %v", err, optionErr)
		}
		if split && bound && device != "lo" || !split && (bound || device != "") {
			t.Fatalf("split=%v bound=%v actual device=%q", split, bound, device)
		}
		if split && !bound {
			t.Skip("kernel refused unprivileged SO_BINDTODEVICE; documented IP/Host/bearer fallback")
		}
	}
}
