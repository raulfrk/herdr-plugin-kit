//go:build !linux

package servicehost

import "errors"

type lifecycleLock struct{}

func acquireLifecycleLock(string, uint32) (*lifecycleLock, error) {
	return nil, errors.New("servicehost: user-service lifecycle requires Linux")
}

func (*lifecycleLock) Close() error { return nil }
