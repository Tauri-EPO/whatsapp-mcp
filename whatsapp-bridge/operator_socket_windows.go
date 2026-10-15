package main

import (
	"errors"
	"net"
)

func validateOperatorSocket(path, _, _ string) error {
	if path != "" {
		return errors.New("WHATSAPP_OPERATOR_SOCKET requires a Unix platform")
	}
	return nil
}

func listenOperatorSocket(_ string) (net.Listener, error) {
	return nil, errors.New("WHATSAPP_OPERATOR_SOCKET requires a Unix platform")
}
