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
		// Read the same kernel binding as an integer: QEMU translates SOL_SOCKET
		// getsockopt as an int and cannot read SO_BINDTODEVICE's string.
		var device int
		var optionErr error
		err = connection.Control(func(fd uintptr) {
			device, optionErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX)
		})
		_ = listener.Close()
		if err != nil || optionErr != nil {
			t.Fatalf("read real socket option: %v %v", err, optionErr)
		}
		loopback, err := net.InterfaceByName("lo")
		if err != nil {
			t.Fatal(err)
		}
		if split && bound && device != loopback.Index || !split && (bound || device != 0) {
			t.Fatalf("split=%v bound=%v actual interface=%d", split, bound, device)
		}
		t.Logf("split=%v bound=%v kernel interface=%d", split, bound, device)
		if split && !bound {
			t.Skip("kernel refused unprivileged SO_BINDTODEVICE; documented IP/Host/bearer fallback")
		}
	}
}
