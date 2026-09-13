// Package herdrapi implements the bounded one-request Herdr socket protocol.
package herdrapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
)

const MaxMessageBytes = 1 << 20

type Caller interface {
	Call(context.Context, string, string, string, any, any) error
}

type Client struct{}

func (Client) Call(ctx context.Context, socketPath, requestID, method string, params, result any) error {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath || strings.ContainsRune(socketPath, 0) {
		return errors.New("Herdr socket path must be clean, absolute, and NUL-free")
	}
	if requestID == "" || method == "" || strings.ContainsAny(requestID+method, "\x00\r\n") {
		return errors.New("invalid Herdr request identity")
	}
	request, err := json.Marshal(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{requestID, method, params})
	if err != nil {
		return fmt.Errorf("encode Herdr request: %w", err)
	}
	if len(request)+1 > MaxMessageBytes {
		return errors.New("Herdr request exceeds size limit")
	}
	request = append(request, '\n')

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect Herdr socket: %w", err)
	}
	defer conn.Close()
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-cancelDone:
		}
	}()
	defer close(cancelDone)

	if _, err := io.Copy(conn, bytes.NewReader(request)); err != nil {
		return fmt.Errorf("write Herdr request: %w", err)
	}
	line, err := bufio.NewReaderSize(conn, MaxMessageBytes+1).ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return errors.New("Herdr response exceeds size limit")
		}
		return fmt.Errorf("read Herdr response: %w", err)
	}
	if len(line) > MaxMessageBytes {
		return errors.New("Herdr response exceeds size limit")
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	var response struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("decode Herdr response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("Herdr response must contain exactly one JSON value")
	}
	if response.ID != requestID {
		return errors.New("Herdr response request identity changed")
	}
	if len(response.Error) != 0 && !bytes.Equal(response.Error, []byte("null")) {
		return fmt.Errorf("Herdr request failed: %s", response.Error)
	}
	if len(response.Result) == 0 {
		return errors.New("Herdr response is missing result")
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode Herdr result: %w", err)
	}
	return nil
}
