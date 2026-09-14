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

type RequestState uint8

const (
	RequestNotWritten RequestState = iota
	RequestWriteStarted
	RequestWritten
	RequestAcknowledged
)

type Receipt struct {
	State RequestState
}

type Client struct{}

func (Client) Call(ctx context.Context, socketPath, requestID, method string, params, result any) error {
	_, err := (Client{}).CallWithReceipt(ctx, socketPath, requestID, method, params, result)
	return err
}

// CallWithReceipt reports how far a single request progressed. Callers that
// must not retry can distinguish a failure before any request byte was written
// from a failure whose server-side outcome is unknown.
func (Client) CallWithReceipt(ctx context.Context, socketPath, requestID, method string, params, result any) (Receipt, error) {
	receipt := Receipt{State: RequestNotWritten}
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath || strings.ContainsRune(socketPath, 0) {
		return receipt, errors.New("Herdr socket path must be clean, absolute, and NUL-free")
	}
	if requestID == "" || method == "" || strings.ContainsAny(requestID+method, "\x00\r\n") {
		return receipt, errors.New("invalid Herdr request identity")
	}
	request, err := json.Marshal(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{requestID, method, params})
	if err != nil {
		return receipt, fmt.Errorf("encode Herdr request: %w", err)
	}
	if len(request)+1 > MaxMessageBytes {
		return receipt, errors.New("Herdr request exceeds size limit")
	}
	request = append(request, '\n')

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return receipt, fmt.Errorf("connect Herdr socket: %w", err)
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

	written, err := io.Copy(conn, bytes.NewReader(request))
	if written > 0 {
		receipt.State = RequestWriteStarted
	}
	if err != nil {
		return receipt, fmt.Errorf("write Herdr request: %w", err)
	}
	if written != int64(len(request)) {
		return receipt, errors.New("write Herdr request: short write")
	}
	receipt.State = RequestWritten
	line, err := bufio.NewReaderSize(conn, MaxMessageBytes+1).ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return receipt, errors.New("Herdr response exceeds size limit")
		}
		return receipt, fmt.Errorf("read Herdr response: %w", err)
	}
	if len(line) > MaxMessageBytes {
		return receipt, errors.New("Herdr response exceeds size limit")
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
		return receipt, fmt.Errorf("decode Herdr response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return receipt, errors.New("Herdr response must contain exactly one JSON value")
	}
	if response.ID != requestID {
		return receipt, errors.New("Herdr response request identity changed")
	}
	if len(response.Error) != 0 && !bytes.Equal(response.Error, []byte("null")) {
		return receipt, fmt.Errorf("Herdr request failed: %s", response.Error)
	}
	if len(response.Result) == 0 {
		return receipt, errors.New("Herdr response is missing result")
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return receipt, fmt.Errorf("decode Herdr result: %w", err)
	}
	receipt.State = RequestAcknowledged
	return receipt, nil
}
