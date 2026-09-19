//go:build linux

package servicehost

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/credentials"
)

type unixPeerAuth struct {
	credentials.CommonAuthInfo
	UID uint32
}

func (unixPeerAuth) AuthType() string { return "unix-peer-uid" }

type unixPeerCredentials struct{ uid uint32 }

func (c unixPeerCredentials) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c.handshake(conn)
}

func (c unixPeerCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return c.handshake(conn)
}

func (c unixPeerCredentials) handshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	uid, err := unixSocketPeerUID(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if uid != c.uid {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("servicehost: peer UID %d does not match service UID %d", uid, c.uid)
	}
	return conn, unixPeerAuth{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}, UID: uid}, nil
}

func (c unixPeerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "unix-peer-uid"}
}

func (c unixPeerCredentials) Clone() credentials.TransportCredentials { return c }
func (unixPeerCredentials) OverrideServerName(string) error           { return nil }

func unixSocketPeerUID(conn net.Conn) (uint32, error) {
	raw, ok := conn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return 0, fmt.Errorf("servicehost: connection %T has no syscall access", conn)
	}
	rawConn, err := raw.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("servicehost: access peer socket: %w", err)
	}
	credential, err := readPeerCredential(rawConn)
	if err != nil {
		return 0, err
	}
	return credential.Uid, nil
}

func readPeerCredential(rawConn syscall.RawConn) (*unix.Ucred, error) {
	var credential *unix.Ucred
	var socketErr error
	controlErr := rawConn.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil {
		return nil, fmt.Errorf("servicehost: inspect peer socket: %w", controlErr)
	}
	if socketErr != nil {
		return nil, fmt.Errorf("servicehost: read peer credentials: %w", socketErr)
	}
	return credential, nil
}
