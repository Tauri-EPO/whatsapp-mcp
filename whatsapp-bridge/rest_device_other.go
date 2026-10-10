//go:build !linux

package main

import "net"

func listenREST(bind string, port int, _ bool) (net.Listener, bool, error) {
	listener, err := net.Listen("tcp", listenAddr(bind, port))
	return listener, false, err
}
