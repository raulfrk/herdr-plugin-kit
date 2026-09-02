package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestStrictEnvelopeDecoding(t *testing.T) {
	now := time.Now()
	valid := request(json.RawMessage(`{"document":"README.md"}`))
	valid.Deadline = now.Add(time.Hour)
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRequest(context.Background(), bytes.NewReader(encoded), now); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for name, input := range map[string][]byte{
		"unknown field":  append(encoded[:len(encoded)-1], []byte(`,"future":true}`)...),
		"trailing value": append(append([]byte(nil), encoded...), []byte(` {}`)...),
		"oversized":      bytes.Repeat([]byte{' '}, MaxEnvelopeBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRequest(context.Background(), bytes.NewReader(input), now); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}

	validResponse, err := json.Marshal(Response{Version: Version, CorrelationID: valid.CorrelationID, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{
		"unknown field":  append(validResponse[:len(validResponse)-1], []byte(`,"future":true}`)...),
		"trailing value": append(append([]byte(nil), validResponse...), []byte(` {}`)...),
		"oversized":      bytes.Repeat([]byte{' '}, MaxEnvelopeBytes+1),
	} {
		t.Run("response "+name, func(t *testing.T) {
			if _, err := DecodeResponse(context.Background(), bytes.NewReader(input)); err == nil {
				t.Fatal("invalid response envelope accepted")
			}
		})
	}
}

func TestDispatchEnforcesEnvelopeDeadline(t *testing.T) {
	req := request(nil)
	req.Deadline = time.Now().Add(20 * time.Millisecond)
	started := time.Now()
	response := Dispatch(context.Background(), req, started, HandlerFunc(func(ctx context.Context, req Request) Response {
		<-ctx.Done()
		return Response{Version: Version, CorrelationID: req.CorrelationID, Error: &CallError{Category: DeadlineExceeded, Message: ctx.Err().Error()}}
	}))
	if response.Error == nil || response.Error.Category != DeadlineExceeded {
		t.Fatalf("response = %+v", response)
	}
	if elapsed := time.Since(started); elapsed < 10*time.Millisecond || elapsed > time.Second {
		t.Fatalf("dispatch deadline elapsed in %v", elapsed)
	}
}

func TestDispatchRejectsInvalidRequestAndHandlerResponse(t *testing.T) {
	invalid := request(nil)
	invalid.Method = "../bad"
	response := Dispatch(context.Background(), invalid, time.Now(), HandlerFunc(func(context.Context, Request) Response {
		t.Fatal("handler called for invalid request")
		return Response{}
	}))
	if response.Error == nil || response.Error.Category != InvalidRequest {
		t.Fatalf("invalid request response = %+v", response)
	}

	valid := request(nil)
	response = Dispatch(context.Background(), valid, time.Now(), HandlerFunc(func(context.Context, Request) Response {
		return Response{Version: Version, CorrelationID: "different"}
	}))
	if response.CorrelationID != valid.CorrelationID || response.Error == nil || response.Error.Category != Internal {
		t.Fatalf("mismatched correlation response = %+v", response)
	}

	response = Dispatch(context.Background(), valid, time.Now(), HandlerFunc(func(context.Context, Request) Response {
		return Response{Version: Version + 1, CorrelationID: valid.CorrelationID}
	}))
	if response.CorrelationID != valid.CorrelationID || response.Error == nil || response.Error.Category != Internal {
		t.Fatalf("invalid handler response = %+v", response)
	}
}

func request(payload json.RawMessage) Request {
	return Request{Version: Version, TargetPlugin: "target.plugin", Interface: "document.open", InterfaceVersion: 1, Method: "open", CorrelationID: "corr-1", Deadline: time.Now().Add(time.Hour), Payload: payload}
}

func TestEnvelopeCancellationAndTypedErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ValidateRequest(ctx, request(nil), time.Now())
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Category != Canceled {
		t.Fatalf("error = %v", err)
	}

	response := Response{Version: Version, CorrelationID: "corr-1", Payload: json.RawMessage(`{}`), Error: &CallError{Category: Internal, Message: "failed"}}
	if err := ValidateResponse(context.Background(), response); err == nil {
		t.Fatal("payload plus error accepted")
	}
}

func TestRequestValidationBoundaries(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"expired deadline", func(r *Request) { r.Deadline = now }},
		{"invalid payload", func(r *Request) { r.Payload = json.RawMessage(`{`) }},
		{"wrong version", func(r *Request) { r.Version++ }},
		{"invalid target", func(r *Request) { r.TargetPlugin = "../target" }},
		{"empty correlation", func(r *Request) { r.CorrelationID = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := request(nil)
			tc.mutate(&r)
			if err := ValidateRequest(context.Background(), r, now); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestResponseValidationBoundaries(t *testing.T) {
	tests := []Response{
		{Version: Version, CorrelationID: "corr-1", Payload: json.RawMessage(`{`)},
		{Version: Version, CorrelationID: "corr-1", Error: &CallError{Category: ErrorCategory("future"), Message: "x"}},
		{Version: Version, CorrelationID: "corr-1", Error: &CallError{Category: Internal, Message: ""}},
	}
	for _, response := range tests {
		if err := ValidateResponse(context.Background(), response); err == nil {
			t.Fatalf("invalid response accepted: %+v", response)
		}
	}
	response := Response{Version: Version, CorrelationID: "corr-1", Payload: make(json.RawMessage, MaxPayloadBytes+1)}
	if err := ValidateResponse(context.Background(), response); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestHandlerReceivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := HandlerFunc(func(ctx context.Context, _ Request) Response {
		if ctx.Err() == nil {
			t.Fatal("handler did not receive canceled context")
		}
		return Response{Version: Version, CorrelationID: "corr-1"}
	})
	handler.Handle(ctx, request(nil))
}

func TestPropertyPayloadBound(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		extra := rapid.IntRange(-2, 2).Draw(t, "offset")
		size := MaxPayloadBytes + extra
		payload := make(json.RawMessage, size)
		for i := range payload {
			payload[i] = ' '
		}
		if size > 0 {
			payload[0], payload[size-1] = '[', ']'
		}
		err := ValidateRequest(context.Background(), request(payload), time.Now())
		if (size <= MaxPayloadBytes) != (err == nil) {
			t.Fatalf("size=%d err=%v", size, err)
		}
	})
}
