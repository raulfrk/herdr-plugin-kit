//go:build !linux

package keybindinghost

import (
	"context"
	"io"

	"github.com/raulfrk/herdr-plugin-kit/documentstore"
)

func acquireTransactionLock(context.Context, string) (io.Closer, error) {
	return nil, documentstore.ErrUnsupported
}
