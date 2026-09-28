//go:build !linux

package documentstore

import "context"

type Store struct{}

func Open(string) (*Store, error) { return nil, ErrUnsupported }
func (*Store) Close() error       { return ErrUnsupported }
func (*Store) Read(context.Context, string) (Document, error) {
	return Document{}, ErrUnsupported
}
func (*Store) ReadBounded(context.Context, string, int) (Document, error) {
	return Document{}, ErrUnsupported
}
func (*Store) Write(context.Context, string, []byte, Revision) (Revision, error) {
	return Revision{}, ErrUnsupported
}
func (*Store) WriteBounded(context.Context, string, []byte, Revision, int) (Revision, error) {
	return Revision{}, ErrUnsupported
}
func (*Store) WritePrivate(context.Context, string, []byte, Revision) (Revision, error) {
	return Revision{}, ErrUnsupported
}

func (*Store) RemoveBounded(context.Context, string, Revision, int) (bool, error) {
	return false, ErrUnsupported
}
