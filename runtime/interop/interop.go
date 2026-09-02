// Package interop defines a small versioned contract for plugin-to-plugin calls.
package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/manifest"
)

const (
	Version         = 1
	MaxPayloadBytes = 1 << 20
	// MaxEnvelopeBytes leaves 16 KiB for metadata around a maximum payload.
	MaxEnvelopeBytes = 1_064_960
	MaxCorrelationID = 128
)

type Request struct {
	Version          uint32          `json:"version"`
	TargetPlugin     string          `json:"target_plugin"`
	Interface        string          `json:"interface"`
	InterfaceVersion uint32          `json:"interface_version"`
	Method           string          `json:"method"`
	CorrelationID    string          `json:"correlation_id"`
	Deadline         time.Time       `json:"deadline"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

type Response struct {
	Version       uint32          `json:"version"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         *CallError      `json:"error,omitempty"`
}

type ErrorCategory string

const (
	InvalidRequest   ErrorCategory = "invalid_request"
	NotFound         ErrorCategory = "not_found"
	Unavailable      ErrorCategory = "unavailable"
	DeadlineExceeded ErrorCategory = "deadline_exceeded"
	Canceled         ErrorCategory = "canceled"
	PayloadTooLarge  ErrorCategory = "payload_too_large"
	Internal         ErrorCategory = "internal"
)

type CallError struct {
	Category ErrorCategory `json:"category"`
	Message  string        `json:"message"`
}

func (e *CallError) Error() string { return string(e.Category) + ": " + e.Message }

// Caller abstracts an in-process, socket, or other transport without prescribing one.
type Caller interface {
	Call(context.Context, Request) (Response, error)
}

// Handler handles one already-routed request. Implementations must honor ctx cancellation.
type Handler interface {
	Handle(context.Context, Request) Response
}
type HandlerFunc func(context.Context, Request) Response

func (f HandlerFunc) Handle(ctx context.Context, req Request) Response { return f(ctx, req) }

func DecodeRequest(ctx context.Context, reader io.Reader, now time.Time) (Request, error) {
	var request Request
	if err := decodeStrict(reader, &request); err != nil {
		return Request{}, err
	}
	if err := ValidateRequest(ctx, request, now); err != nil {
		return Request{}, err
	}
	return request, nil
}

func DecodeResponse(ctx context.Context, reader io.Reader) (Response, error) {
	var response Response
	if err := decodeStrict(reader, &response); err != nil {
		return Response{}, err
	}
	if err := ValidateResponse(ctx, response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// Dispatch applies the envelope deadline to handler execution and validates
// both sides of the transport-neutral call contract.
func Dispatch(ctx context.Context, request Request, now time.Time, handler Handler) Response {
	response := Response{Version: Version, CorrelationID: request.CorrelationID}
	if handler == nil {
		response.Error = &CallError{Category: Internal, Message: "nil handler"}
		return response
	}
	if err := ValidateRequest(ctx, request, now); err != nil {
		response.Error = toCallError(err, InvalidRequest)
		return response
	}
	callCtx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	response = handler.Handle(callCtx, request)
	if err := ValidateResponse(callCtx, response); err != nil {
		return Response{Version: Version, CorrelationID: request.CorrelationID, Error: toCallError(err, Internal)}
	}
	if response.CorrelationID != request.CorrelationID {
		return Response{Version: Version, CorrelationID: request.CorrelationID, Error: &CallError{Category: Internal, Message: "handler changed correlation_id"}}
	}
	return response
}

func ValidateRequest(ctx context.Context, req Request, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if req.Version != Version {
		return fmt.Errorf("unsupported envelope version %d", req.Version)
	}
	if err := manifest.ValidateID("target_plugin", req.TargetPlugin); err != nil {
		return err
	}
	if err := manifest.ValidateID("interface", req.Interface); err != nil {
		return err
	}
	if req.InterfaceVersion == 0 {
		return errors.New("interface_version must be positive")
	}
	if err := manifest.ValidateID("method", req.Method); err != nil {
		return err
	}
	if err := validateCorrelation(req.CorrelationID); err != nil {
		return err
	}
	if req.Deadline.IsZero() || !req.Deadline.After(now) {
		return &CallError{Category: DeadlineExceeded, Message: "deadline has elapsed"}
	}
	if err := validatePayload(req.Payload); err != nil {
		return err
	}
	return nil
}

func ValidateResponse(ctx context.Context, response Response) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if response.Version != Version {
		return fmt.Errorf("unsupported envelope version %d", response.Version)
	}
	if err := validateCorrelation(response.CorrelationID); err != nil {
		return err
	}
	if response.Error != nil && len(response.Payload) != 0 {
		return errors.New("response cannot contain both payload and error")
	}
	if response.Error != nil {
		if !validCategory(response.Error.Category) || strings.TrimSpace(response.Error.Message) == "" || len(response.Error.Message) > 1024 {
			return errors.New("response has invalid typed error")
		}
	}
	return validatePayload(response.Payload)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return &CallError{Category: InvalidRequest, Message: "nil context"}
	}
	if err := ctx.Err(); err != nil {
		category := Canceled
		if errors.Is(err, context.DeadlineExceeded) {
			category = DeadlineExceeded
		}
		return &CallError{Category: category, Message: err.Error()}
	}
	return nil
}

func validatePayload(payload json.RawMessage) error {
	if len(payload) > MaxPayloadBytes {
		return &CallError{Category: PayloadTooLarge, Message: "payload exceeds limit"}
	}
	if len(payload) != 0 && !json.Valid(payload) {
		return &CallError{Category: InvalidRequest, Message: "payload is not valid JSON"}
	}
	return nil
}
func validateCorrelation(value string) error {
	if value == "" || len(value) > MaxCorrelationID {
		return errors.New("correlation_id must be 1-128 bytes")
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return errors.New("correlation_id must contain printable ASCII without spaces")
		}
	}
	return nil
}
func validCategory(c ErrorCategory) bool {
	switch c {
	case InvalidRequest, NotFound, Unavailable, DeadlineExceeded, Canceled, PayloadTooLarge, Internal:
		return true
	}
	return false
}

func decodeStrict(reader io.Reader, target any) error {
	if reader == nil {
		return &CallError{Category: InvalidRequest, Message: "nil envelope reader"}
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxEnvelopeBytes+1))
	if err != nil {
		return fmt.Errorf("read envelope: %w", err)
	}
	if len(data) > MaxEnvelopeBytes {
		return &CallError{Category: PayloadTooLarge, Message: "envelope exceeds limit"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &CallError{Category: InvalidRequest, Message: "invalid envelope JSON"}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return &CallError{Category: InvalidRequest, Message: "envelope must contain exactly one JSON value"}
	}
	return nil
}

func toCallError(err error, fallback ErrorCategory) *CallError {
	var callErr *CallError
	if errors.As(err, &callErr) {
		return callErr
	}
	return &CallError{Category: fallback, Message: err.Error()}
}
