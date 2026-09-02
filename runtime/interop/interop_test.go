package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	decodedRequest, err := DecodeRequest(context.Background(), bytes.NewReader(encoded), now)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if decodedRequest.CorrelationID != valid.CorrelationID || !bytes.Equal(decodedRequest.Payload, valid.Payload) {
		t.Fatalf("decoded request = %+v", decodedRequest)
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
	decodedResponse, err := DecodeResponse(context.Background(), bytes.NewReader(validResponse))
	if err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if decodedResponse.CorrelationID != valid.CorrelationID || !bytes.Equal(decodedResponse.Payload, json.RawMessage(`{}`)) {
		t.Fatalf("decoded response = %+v", decodedResponse)
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

func TestEnvelopeSizeContract(t *testing.T) {
	const expectedMaxEnvelopeBytes = (1 << 20) + (16 << 10)
	if MaxEnvelopeBytes != expectedMaxEnvelopeBytes {
		t.Fatalf("MaxEnvelopeBytes = %d, want %d", MaxEnvelopeBytes, expectedMaxEnvelopeBytes)
	}

	type paddedEnvelope struct {
		Padding string `json:"padding"`
	}
	prefix := []byte(`{"padding":"`)
	suffix := []byte(`"}`)
	exact := append(append(append([]byte(nil), prefix...), bytes.Repeat([]byte{'x'}, expectedMaxEnvelopeBytes-len(prefix)-len(suffix))...), suffix...)
	var decoded paddedEnvelope
	if err := decodeStrict(bytes.NewReader(exact), &decoded); err != nil {
		t.Fatalf("exactly maximum-sized envelope rejected: %v", err)
	}
	if len(decoded.Padding) != expectedMaxEnvelopeBytes-len(prefix)-len(suffix) {
		t.Fatalf("decoded padding length = %d", len(decoded.Padding))
	}

	tooLarge := append(append([]byte(nil), exact...), ' ')
	err := decodeStrict(bytes.NewReader(tooLarge), &decoded)
	assertCallErrorCategory(t, err, PayloadTooLarge)
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
	assertCallErrorCategory(t, err, Canceled)

	response := Response{Version: Version, CorrelationID: "corr-1", Payload: json.RawMessage(`{}`), Error: &CallError{Category: Internal, Message: "failed"}}
	if err := ValidateResponse(context.Background(), response); err == nil {
		t.Fatal("payload plus error accepted")
	}
}

func TestCallErrorFormatting(t *testing.T) {
	err := &CallError{Category: Unavailable, Message: "plugin is restarting"}
	if got, want := err.Error(), "unavailable: plugin is restarting"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestDecodeAppliesEnvelopeValidation(t *testing.T) {
	now := time.Now()
	expired := request(nil)
	expired.Deadline = now
	encodedRequest, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeRequest(context.Background(), bytes.NewReader(encodedRequest), now)
	assertCallErrorCategory(t, err, DeadlineExceeded)

	invalidResponse, err := json.Marshal(Response{Version: Version + 1, CorrelationID: "corr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeResponse(context.Background(), bytes.NewReader(invalidResponse)); err == nil {
		t.Fatal("response with unsupported version accepted")
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

func TestResponsePayloadAndErrorAlternatives(t *testing.T) {
	responses := []Response{
		{Version: Version, CorrelationID: "corr-1", Payload: json.RawMessage(`{"ok":true}`)},
		{Version: Version, CorrelationID: "corr-1", Error: &CallError{Category: NotFound, Message: "document not found"}},
	}
	for _, response := range responses {
		if err := ValidateResponse(context.Background(), response); err != nil {
			t.Fatalf("valid response rejected: %+v: %v", response, err)
		}
	}
}

func TestTypedErrorMessageBoundaries(t *testing.T) {
	valid := Response{
		Version:       Version,
		CorrelationID: "corr-1",
		Error:         &CallError{Category: Internal, Message: strings.Repeat("x", 1024)},
	}
	if err := ValidateResponse(context.Background(), valid); err != nil {
		t.Fatalf("1024-byte error message rejected: %v", err)
	}

	for name, message := range map[string]string{
		"whitespace only": " \t\n",
		"too long":        strings.Repeat("x", 1025),
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			response.Error = &CallError{Category: Internal, Message: message}
			if err := ValidateResponse(context.Background(), response); err == nil {
				t.Fatal("invalid typed error accepted")
			}
		})
	}
}

func TestCorrelationBoundaries(t *testing.T) {
	for name, correlationID := range map[string]string{
		"single lowest printable byte":  "!",
		"single highest printable byte": "~",
		"maximum length":                strings.Repeat("x", MaxCorrelationID),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCorrelation(correlationID); err != nil {
				t.Fatalf("valid correlation ID rejected: %v", err)
			}
		})
	}

	for name, correlationID := range map[string]string{
		"empty":               "",
		"too long":            strings.Repeat("x", MaxCorrelationID+1),
		"space":               "corr id",
		"below printable":     "\x20",
		"above printable":     "\x7f",
		"non-ASCII printable": "é",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCorrelation(correlationID); err == nil {
				t.Fatal("invalid correlation ID accepted")
			}
		})
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

func assertCallErrorCategory(t *testing.T, err error, want ErrorCategory) {
	t.Helper()
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Category != want {
		t.Fatalf("error = %v, want CallError category %q", err, want)
	}
}
