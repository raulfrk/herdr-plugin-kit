// Package documentstore provides cooperative, revision-checked storage for
// plugin configuration documents.
package documentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var (
	ErrClosed      = errors.New("document store is closed")
	ErrConflict    = errors.New("document revision conflict")
	ErrInvalidPath = errors.New("invalid document path")
	ErrNotRegular  = errors.New("document is not a regular file")
	ErrUnsupported = errors.New("document store is unsupported on this platform")
)

// Revision identifies exact document bytes. The zero value means that a
// checked write expects the document not to exist.
type Revision [sha256.Size]byte

func (r Revision) String() string { return hex.EncodeToString(r[:]) }

// Document is one coherent read of a regular file.
type Document struct {
	Bytes    []byte
	Revision Revision
}

func revision(data []byte) Revision { return sha256.Sum256(data) }
