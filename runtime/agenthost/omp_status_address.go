package agenthost

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

//go:embed integrations/omp-status-address.ts
var ompStatusAddressExtension []byte

// OMPStatusAddressExtension returns a fresh copy of the companion that derives
// a pane-local status socket from Herdr's existing pane environment.
func OMPStatusAddressExtension() []byte {
	return append([]byte(nil), ompStatusAddressExtension...)
}

// DeriveOMPStatusSocket returns the pane-local OMP status socket used by the
// companion extension. Its basename is never longer than Herdr's socket
// basename, so a valid Herdr Unix socket does not become too long.
func DeriveOMPStatusSocket(herdrSocket, paneID string) (string, error) {
	if !filepath.IsAbs(herdrSocket) || filepath.Clean(herdrSocket) != herdrSocket || strings.ContainsRune(herdrSocket, 0) {
		return "", errors.New("Herdr socket must be clean, absolute, and NUL-free")
	}
	if paneID == "" || len(paneID) > maxIdentityBytes || strings.ContainsRune(paneID, 0) {
		return "", errors.New("pane identity is invalid")
	}
	basenameLength := len(filepath.Base(herdrSocket))
	if basenameLength < 9 {
		return "", errors.New("Herdr socket basename is too short")
	}
	digest := sha256.Sum256(append(append([]byte(herdrSocket), 0), paneID...))
	encoded := base64.RawURLEncoding.EncodeToString(digest[:])
	length := basenameLength - 1
	if length > len(encoded) {
		length = len(encoded)
	}
	return filepath.Join(filepath.Dir(herdrSocket), "o"+encoded[:length]), nil
}

func derivePromptOMPStatusSocket(ctx context.Context, host Host, paneID string) (string, error) {
	herdrSocket, err := (sessionhost.Host{Runner: host.Runner, Herdr: host.Herdr, Session: host.Session}).Socket(ctx)
	if err != nil {
		return "", err
	}
	return DeriveOMPStatusSocket(herdrSocket, paneID)
}
