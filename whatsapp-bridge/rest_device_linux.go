package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Bind the split socket to its interface as well as its IP: Linux's weak host
// model otherwise accepts a packet to that IP arriving on the operator NIC.
func listenREST(bind string, port int, split bool) (net.Listener, bool, error) {
	if !split {
		listener, err := net.Listen("tcp", listenAddr(bind, port))
		return listener, false, err
	}
	ip := net.ParseIP(bind)
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, false, err
	}
	device := ""
	for _, iface := range interfaces {
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			return nil, false, addrErr
		}
		for _, address := range addresses {
			local, _, parseErr := net.ParseCIDR(address.String())
			if parseErr == nil && ip != nil && ip.Equal(local) {
				if device != "" && device != iface.Name {
					return nil, false, errors.New("split REST IP belongs to multiple interfaces")
				}
				device = iface.Name
			}
		}
	}
	if device == "" {
		return nil, false, errors.New("split REST IP has no local interface")
	}
	deviceBound := false
	config := net.ListenConfig{Control: func(_, _ string, connection syscall.RawConn) error {
		var optionErr error
		if controlErr := connection.Control(func(fd uintptr) {
			optionErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
		}); controlErr != nil {
			return controlErr
		}
		if errors.Is(optionErr, unix.EPERM) || errors.Is(optionErr, unix.EACCES) || errors.Is(optionErr, unix.ENOPROTOOPT) {
			// No extra capability is granted. Preserve the IP/Host/bearer checks
			// and report the documented residual on kernels denying this option.
			return nil
		}
		if optionErr != nil {
			return fmt.Errorf("bind split REST to agent interface: %w", optionErr)
		}
		deviceBound = true
		return nil
	}}
	listener, err := config.Listen(context.Background(), "tcp", listenAddr(bind, port))
	return listener, deviceBound, err
}
