//go:build !linux

package servicehost

import "errors"

type socketLock struct{}

func acquireSocketLock(string, uint32) (*socketLock, error) {
	return nil, errors.New("servicehost: private service sockets require Linux")
}

func (*socketLock) Close() error { return nil }
