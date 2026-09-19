//go:build !linux

package servicehost

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc/credentials"
)

type unixPeerCredentials struct{ uid uint32 }

func (unixPeerCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("servicehost: same-UID Unix transport requires Linux")
}

func (unixPeerCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("servicehost: same-UID Unix transport requires Linux")
}

func (unixPeerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "unix-peer-uid"}
}

func (c unixPeerCredentials) Clone() credentials.TransportCredentials { return c }
func (unixPeerCredentials) OverrideServerName(string) error           { return nil }
