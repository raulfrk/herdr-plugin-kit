//go:build !linux

package documentstore

import "context"

type Store struct{}

func Open(string) (*Store, error) { return nil, ErrUnsupported }
func (*Store) Close() error       { return ErrUnsupported }
func (*Store) Read(context.Context, string) (Document, error) {
	return Document{}, ErrUnsupported
}
func (*Store) Write(context.Context, string, []byte, Revision) (Revision, error) {
	return Revision{}, ErrUnsupported
}
func (*Store) WritePrivate(context.Context, string, []byte, Revision) (Revision, error) {
	return Revision{}, ErrUnsupported
}
