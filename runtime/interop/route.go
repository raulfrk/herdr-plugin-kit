package interop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/manifest"
)

const (
	ActionRouteInterface               = "action-route"
	ActionRouteInterfaceVersion uint32 = 1
	ActionRouteActionID                = "route"
	MaxRouteActions                    = 256
	MaxRouteKeys                       = 8
	MaxRouteSiblings                   = 36
	MaxRouteEnvelopeBytes              = 64 << 10
	maxRouteLabelBytes                 = 128
	maxRouteReasonBytes                = 512
	maxRouteRevisionBytes              = 128
	maxRouteOriginBytes                = 128
)

const (
	RouteMethodCatalog = "catalog"
	RouteMethodInvoke  = "invoke"
)

type RouteOrigin struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Tab       string `json:"tab"`
	Pane      string `json:"pane"`
}

type RouteAction struct {
	ActionID      string   `json:"action_id"`
	Label         string   `json:"label"`
	SuggestedPath []string `json:"suggested_path"`
	Available     bool     `json:"available"`
	Reason        string   `json:"reason,omitempty"`
}

type RouteCatalog struct {
	RouteRevision string        `json:"route_revision"`
	Actions       []RouteAction `json:"actions"`
}

type RouteInvocation struct {
	Origin        RouteOrigin
	RouteRevision string
	ActionID      string
	Keys          []string
}

type RouteOutcome string

const (
	RouteCompleted RouteOutcome = "completed"
	RouteRejected  RouteOutcome = "rejected"
	RouteFailed    RouteOutcome = "failed"
)

type RouteFocus string

const (
	RouteFocusPreserved RouteFocus = "preserved"
	RouteFocusChanged   RouteFocus = "changed"
)

type RouteExecution struct {
	Outcome RouteOutcome `json:"outcome"`
	Focus   RouteFocus   `json:"focus"`
}

type RouteProvider interface {
	Catalog(context.Context, RouteOrigin) (RouteCatalog, error)
	Invoke(context.Context, RouteInvocation) (RouteExecution, error)
}

type RouteCatalogCall struct {
	TargetPlugin string
	Origin       RouteOrigin
	Deadline     time.Time
}

type RouteCatalogResult struct {
	TargetPlugin  string        `json:"target_plugin"`
	RouteRevision string        `json:"route_revision"`
	Actions       []RouteAction `json:"actions"`
}

type RouteInvokeCall struct {
	TargetPlugin  string
	Origin        RouteOrigin
	RouteRevision string
	ActionID      string
	Keys          []string
	Deadline      time.Time
}

type RouteInvokeResult struct {
	TargetPlugin  string       `json:"target_plugin"`
	RouteRevision string       `json:"route_revision"`
	ActionID      string       `json:"action_id"`
	Outcome       RouteOutcome `json:"outcome"`
	Focus         RouteFocus   `json:"focus"`
}

type routeCatalogPayload struct {
	Origin RouteOrigin `json:"origin"`
}

type routeInvokePayload struct {
	Origin        RouteOrigin `json:"origin"`
	RouteRevision string      `json:"route_revision"`
	ActionID      string      `json:"action_id"`
	Keys          []string    `json:"keys"`
}

type RouteClient struct {
	Caller         Caller
	NewCorrelation func() (string, error)
}

func (c RouteClient) Catalog(ctx context.Context, call RouteCatalogCall) (RouteCatalogResult, error) {
	if c.Caller == nil {
		return RouteCatalogResult{}, errors.New("action-route requires a caller")
	}
	if err := ValidateRouteOrigin(call.Origin); err != nil {
		return RouteCatalogResult{}, err
	}
	payload, err := json.Marshal(routeCatalogPayload{Origin: call.Origin})
	if err != nil {
		return RouteCatalogResult{}, err
	}
	request, err := c.request(call.TargetPlugin, RouteMethodCatalog, call.Deadline, payload)
	if err != nil {
		return RouteCatalogResult{}, err
	}
	response, err := c.Caller.Call(ctx, request)
	if err != nil {
		return RouteCatalogResult{}, err
	}
	if err := validateRouteEnvelopeResponse(ctx, request, response); err != nil {
		return RouteCatalogResult{}, err
	}
	if response.Error != nil {
		return RouteCatalogResult{}, response.Error
	}
	var result RouteCatalogResult
	if err := decodeRouteCatalogResult(response.Payload, &result); err != nil {
		return RouteCatalogResult{}, err
	}
	if result.TargetPlugin != call.TargetPlugin {
		return RouteCatalogResult{}, errors.New("catalog target identity changed")
	}
	if err := ValidateRouteCatalog(RouteCatalog{RouteRevision: result.RouteRevision, Actions: result.Actions}); err != nil {
		return RouteCatalogResult{}, err
	}
	return result, nil
}

func (c RouteClient) Invoke(ctx context.Context, call RouteInvokeCall) (RouteInvokeResult, error) {
	if c.Caller == nil {
		return RouteInvokeResult{}, errors.New("action-route requires a caller")
	}
	invocation := RouteInvocation{Origin: call.Origin, RouteRevision: call.RouteRevision, ActionID: call.ActionID, Keys: append([]string(nil), call.Keys...)}
	if err := ValidateRouteInvocation(invocation); err != nil {
		return RouteInvokeResult{}, err
	}
	payload, err := json.Marshal(routeInvokePayload(invocation))
	if err != nil {
		return RouteInvokeResult{}, err
	}
	request, err := c.request(call.TargetPlugin, RouteMethodInvoke, call.Deadline, payload)
	if err != nil {
		return RouteInvokeResult{}, err
	}
	response, err := c.Caller.Call(ctx, request)
	if err != nil {
		return RouteInvokeResult{}, err
	}
	if err := validateRouteEnvelopeResponse(ctx, request, response); err != nil {
		return RouteInvokeResult{}, err
	}
	if response.Error != nil {
		return RouteInvokeResult{}, response.Error
	}
	var result RouteInvokeResult
	if err := decodeRouteInvokeResult(response.Payload, &result); err != nil {
		return RouteInvokeResult{}, err
	}
	if result.TargetPlugin != call.TargetPlugin || result.RouteRevision != call.RouteRevision || result.ActionID != call.ActionID {
		return RouteInvokeResult{}, errors.New("invoke response identity changed")
	}
	if err := ValidateRouteExecution(RouteExecution{Outcome: result.Outcome, Focus: result.Focus}); err != nil {
		return RouteInvokeResult{}, err
	}
	return result, nil
}

func (c RouteClient) request(target, method string, deadline time.Time, payload []byte) (Request, error) {
	correlation := c.NewCorrelation
	if correlation == nil {
		correlation = newRouteCorrelation
	}
	id, err := correlation()
	if err != nil {
		return Request{}, fmt.Errorf("create correlation: %w", err)
	}
	request := Request{Version: Version, TargetPlugin: target, Interface: ActionRouteInterface, InterfaceVersion: ActionRouteInterfaceVersion, Method: method, CorrelationID: id, Deadline: deadline, Payload: payload}
	if err := validateRouteRequest(context.Background(), request, time.Now()); err != nil {
		return Request{}, err
	}
	return request, nil
}

func newRouteCorrelation() (string, error) {
	return rand.Text(), nil
}

func DispatchRoute(ctx context.Context, request Request, now time.Time, provider RouteProvider) Response {
	return Dispatch(ctx, request, now, HandlerFunc(func(callCtx context.Context, request Request) Response {
		response := Response{Version: Version, CorrelationID: request.CorrelationID}
		if provider == nil {
			response.Error = &CallError{Category: Internal, Message: "nil action-route provider"}
			return response
		}
		if err := validateRouteRequest(callCtx, request, now); err != nil {
			response.Error = toCallError(err, InvalidRequest)
			return response
		}
		switch request.Method {
		case RouteMethodCatalog:
			var payload routeCatalogPayload
			if err := decodeRouteCatalogPayload(request.Payload, &payload); err != nil {
				response.Error = toCallError(err, InvalidRequest)
				return response
			}
			catalog, err := provider.Catalog(callCtx, payload.Origin)
			if err != nil {
				response.Error = routeProviderError(err)
				return response
			}
			if err := ValidateRouteCatalog(catalog); err != nil {
				response.Error = toCallError(err, Internal)
				return response
			}
			response.Payload, err = json.Marshal(RouteCatalogResult{TargetPlugin: request.TargetPlugin, RouteRevision: catalog.RouteRevision, Actions: catalog.Actions})
			if err != nil {
				response.Error = toCallError(err, Internal)
			}
			return response
		case RouteMethodInvoke:
			var payload routeInvokePayload
			if err := decodeRouteInvokePayload(request.Payload, &payload); err != nil {
				response.Error = toCallError(err, InvalidRequest)
				return response
			}
			invocation := RouteInvocation(payload)
			catalog, err := provider.Catalog(callCtx, invocation.Origin)
			if err != nil {
				response.Error = routeProviderError(err)
				return response
			}
			if err := ValidateRouteCatalog(catalog); err != nil {
				response.Error = toCallError(err, Internal)
				return response
			}
			if catalog.RouteRevision != invocation.RouteRevision {
				response.Error = &CallError{Category: Unavailable, Message: "action-route catalog revision changed"}
				return response
			}
			var selected *RouteAction
			for index := range catalog.Actions {
				if catalog.Actions[index].ActionID == invocation.ActionID {
					selected = &catalog.Actions[index]
					break
				}
			}
			if selected == nil {
				response.Error = &CallError{Category: NotFound, Message: "action-route action is not in the current catalog"}
				return response
			}
			if !selected.Available {
				message := "action-route action is unavailable"
				if selected.Reason != "" {
					message += ": " + selected.Reason
				}
				response.Error = &CallError{Category: Unavailable, Message: message}
				return response
			}
			execution, err := provider.Invoke(callCtx, invocation)
			if err != nil {
				response.Error = routeProviderError(err)
				return response
			}
			if err := ValidateRouteExecution(execution); err != nil {
				response.Error = toCallError(err, Internal)
				return response
			}
			response.Payload, err = json.Marshal(RouteInvokeResult{TargetPlugin: request.TargetPlugin, RouteRevision: invocation.RouteRevision, ActionID: invocation.ActionID, Outcome: execution.Outcome, Focus: execution.Focus})
			if err != nil {
				response.Error = toCallError(err, Internal)
			}
			return response
		default:
			response.Error = &CallError{Category: InvalidRequest, Message: "unsupported action-route method"}
			return response
		}
	}))
}

func ServeRoute(ctx context.Context, reader io.Reader, writer io.Writer, now time.Time, provider RouteProvider) error {
	data, err := readRouteEnvelope(reader)
	if err != nil {
		return err
	}
	var request Request
	if err := decodeExactJSON(data, routeRequestFields, &request); err != nil {
		return fmt.Errorf("decode action-route request: %w", err)
	}
	if err := validateCorrelation(request.CorrelationID); err != nil {
		return fmt.Errorf("decode action-route request: %w", err)
	}
	response := DispatchRoute(ctx, request, now, provider)
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(encoded)+1 > MaxRouteEnvelopeBytes {
		response = Response{Version: Version, CorrelationID: request.CorrelationID, Error: &CallError{Category: PayloadTooLarge, Message: "action-route response exceeds its bound"}}
		encoded, err = json.Marshal(response)
		if err != nil {
			return err
		}
	}
	return writeRouteEnvelope(writer, encoded)
}

func writeRouteEnvelope(writer io.Writer, encoded []byte) error {
	if len(encoded)+1 > MaxRouteEnvelopeBytes {
		return errors.New("action-route response exceeds its bound")
	}
	_, err := io.Copy(writer, bytes.NewReader(append(encoded, '\n')))
	return err
}

func validateRouteRequest(ctx context.Context, request Request, now time.Time) error {
	if err := ValidateRequest(ctx, request, now); err != nil {
		return err
	}
	if request.Interface != ActionRouteInterface || request.InterfaceVersion != ActionRouteInterfaceVersion {
		return errors.New("unsupported action-route interface")
	}
	if len(request.Payload) == 0 {
		return errors.New("action-route payload is missing")
	}
	switch request.Method {
	case RouteMethodCatalog:
		var payload routeCatalogPayload
		return decodeRouteCatalogPayload(request.Payload, &payload)
	case RouteMethodInvoke:
		var payload routeInvokePayload
		return decodeRouteInvokePayload(request.Payload, &payload)
	default:
		return errors.New("unsupported action-route method")
	}
}

func routeRequestOrigin(request Request) (RouteOrigin, error) {
	switch request.Method {
	case RouteMethodCatalog:
		var payload routeCatalogPayload
		if err := decodeRouteCatalogPayload(request.Payload, &payload); err != nil {
			return RouteOrigin{}, err
		}
		return payload.Origin, nil
	case RouteMethodInvoke:
		var payload routeInvokePayload
		if err := decodeRouteInvokePayload(request.Payload, &payload); err != nil {
			return RouteOrigin{}, err
		}
		return payload.Origin, nil
	default:
		return RouteOrigin{}, errors.New("unsupported action-route method")
	}
}

func validateRouteEnvelopeResponse(ctx context.Context, request Request, response Response) error {
	if err := ValidateResponse(ctx, response); err != nil {
		return err
	}
	if response.CorrelationID != request.CorrelationID {
		return errors.New("action-route correlation changed")
	}
	return nil
}

func ValidateRouteOrigin(origin RouteOrigin) error {
	for name, value := range map[string]string{"workspace": origin.Workspace, "tab": origin.Tab, "pane": origin.Pane} {
		if err := validateRouteText(name, value, maxRouteOriginBytes, false); err != nil {
			return err
		}
	}
	return validateRouteText("session", origin.Session, maxRouteOriginBytes, true)
}

func ValidateRouteCatalog(catalog RouteCatalog) error {
	if err := validateRouteText("route_revision", catalog.RouteRevision, maxRouteRevisionBytes, false); err != nil {
		return err
	}
	if len(catalog.Actions) > MaxRouteActions {
		return fmt.Errorf("route actions exceed %d entries", MaxRouteActions)
	}
	seenIDs := make(map[string]bool, len(catalog.Actions))
	seenPaths := make(map[string]bool, len(catalog.Actions))
	siblings := make(map[string]map[string]bool)
	for index, action := range catalog.Actions {
		if err := manifest.ValidateID("action_id", action.ActionID); err != nil {
			return fmt.Errorf("actions[%d]: %w", index, err)
		}
		if seenIDs[action.ActionID] {
			return fmt.Errorf("duplicate route action %q", action.ActionID)
		}
		seenIDs[action.ActionID] = true
		if err := validateRouteText("label", action.Label, maxRouteLabelBytes, false); err != nil {
			return fmt.Errorf("actions[%d]: %w", index, err)
		}
		if err := validateRouteText("reason", action.Reason, maxRouteReasonBytes, true); err != nil {
			return fmt.Errorf("actions[%d]: %w", index, err)
		}
		if len(action.SuggestedPath) == 0 || len(action.SuggestedPath) >= MaxRouteKeys {
			return fmt.Errorf("actions[%d]: suggested_path must contain 1-7 participant-relative keys", index)
		}
		for keyIndex, key := range action.SuggestedPath {
			if !validRouteKey(key) {
				return fmt.Errorf("actions[%d].suggested_path[%d] is not a lowercase letter or digit", index, keyIndex)
			}
			prefix := strings.Join(action.SuggestedPath[:keyIndex], "")
			if siblings[prefix] == nil {
				siblings[prefix] = map[string]bool{}
			}
			siblings[prefix][key] = true
			if len(siblings[prefix]) > MaxRouteSiblings {
				return fmt.Errorf("route prefix %q exceeds %d siblings", prefix, MaxRouteSiblings)
			}
		}
		path := strings.Join(action.SuggestedPath, "")
		if seenPaths[path] {
			return fmt.Errorf("duplicate suggested_path %q", path)
		}
		seenPaths[path] = true
	}
	return nil
}

func ValidateRouteInvocation(invocation RouteInvocation) error {
	if err := ValidateRouteOrigin(invocation.Origin); err != nil {
		return err
	}
	if err := validateRouteText("route_revision", invocation.RouteRevision, maxRouteRevisionBytes, false); err != nil {
		return err
	}
	if err := manifest.ValidateID("action_id", invocation.ActionID); err != nil {
		return err
	}
	if len(invocation.Keys) == 0 || len(invocation.Keys) > MaxRouteKeys {
		return fmt.Errorf("keys must contain 1-%d entries", MaxRouteKeys)
	}
	for index, key := range invocation.Keys {
		if !validRouteKey(key) {
			return fmt.Errorf("keys[%d] is not a lowercase letter or digit", index)
		}
	}
	return nil
}

func ValidateRouteExecution(execution RouteExecution) error {
	if execution.Outcome != RouteCompleted && execution.Outcome != RouteRejected && execution.Outcome != RouteFailed {
		return fmt.Errorf("invalid route outcome %q", execution.Outcome)
	}
	if execution.Focus != RouteFocusPreserved && execution.Focus != RouteFocusChanged {
		return fmt.Errorf("invalid route focus %q", execution.Focus)
	}
	return nil
}

func validateRouteText(name, value string, maximum int, optional bool) error {
	if value == "" && optional {
		return nil
	}
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be 1-%d valid UTF-8 NUL-free bytes", name, maximum)
	}
	return nil
}

func validRouteKey(value string) bool {
	return len(value) == 1 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= '0' && value[0] <= '9'))
}

func routeProviderError(err error) *CallError {
	var typed *CallError
	if errors.As(err, &typed) && validCallError(typed) {
		return &CallError{Category: typed.Category, Message: typed.Message}
	}
	return &CallError{Category: Internal, Message: "action-route provider failed"}
}

var (
	routeRequestFields        = []string{"version", "target_plugin", "interface", "interface_version", "method", "correlation_id", "deadline", "payload"}
	routeCatalogPayloadFields = []string{"origin"}
	routeInvokePayloadFields  = []string{"origin", "route_revision", "action_id", "keys"}
	routeOriginFields         = []string{"session", "workspace", "tab", "pane"}
	routeResponseFields       = []string{"version", "correlation_id", "payload", "error"}
	routeCallErrorFields      = []string{"category", "message"}
	routeCatalogResultFields  = []string{"target_plugin", "route_revision", "actions"}
	routeActionFields         = []string{"action_id", "label", "suggested_path", "available", "reason"}
	routeInvokeResultFields   = []string{"target_plugin", "route_revision", "action_id", "outcome", "focus"}
)

func decodeRouteEnvelopeResponse(data []byte, response *Response) error {
	objects, err := exactJSONObject(data, routeResponseFields)
	if err != nil {
		return err
	}
	if raw := objects["error"]; len(raw) != 0 && string(raw) != "null" {
		if _, err := exactJSONObject(raw, routeCallErrorFields); err != nil {
			return fmt.Errorf("error: %w", err)
		}
	}
	return decodeExactJSON(data, routeResponseFields, response)
}

func decodeRouteCatalogPayload(data []byte, payload *routeCatalogPayload) error {
	objects, err := exactJSONObject(data, routeCatalogPayloadFields)
	if err != nil {
		return err
	}
	if _, err := exactJSONObject(objects["origin"], routeOriginFields); err != nil {
		return fmt.Errorf("origin: %w", err)
	}
	if err := decodeExactJSON(data, routeCatalogPayloadFields, payload); err != nil {
		return err
	}
	return ValidateRouteOrigin(payload.Origin)
}

func decodeRouteInvokePayload(data []byte, payload *routeInvokePayload) error {
	objects, err := exactJSONObject(data, routeInvokePayloadFields)
	if err != nil {
		return err
	}
	if _, err := exactJSONObject(objects["origin"], routeOriginFields); err != nil {
		return fmt.Errorf("origin: %w", err)
	}
	if err := decodeExactJSON(data, routeInvokePayloadFields, payload); err != nil {
		return err
	}
	return ValidateRouteInvocation(RouteInvocation(*payload))
}

func decodeRouteCatalogResult(data []byte, result *RouteCatalogResult) error {
	objects, err := exactJSONObject(data, routeCatalogResultFields)
	if err != nil {
		return err
	}
	var actions []json.RawMessage
	if err := json.Unmarshal(objects["actions"], &actions); err != nil {
		return err
	}
	for index, action := range actions {
		if _, err := exactJSONObject(action, routeActionFields); err != nil {
			return fmt.Errorf("actions[%d]: %w", index, err)
		}
	}
	return decodeExactJSON(data, routeCatalogResultFields, result)
}

func decodeRouteInvokeResult(data []byte, result *RouteInvokeResult) error {
	return decodeExactJSON(data, routeInvokeResultFields, result)
}

func readRouteEnvelope(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxRouteEnvelopeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRouteEnvelopeBytes {
		return nil, fmt.Errorf("action-route envelope exceeds %d bytes", MaxRouteEnvelopeBytes)
	}
	return data, nil
}

func decodeExactJSON(data []byte, fields []string, target any) error {
	if len(data) > MaxRouteEnvelopeBytes {
		return fmt.Errorf("action-route JSON exceeds %d bytes", MaxRouteEnvelopeBytes)
	}
	if _, err := exactJSONObject(data, fields); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("action-route JSON must contain exactly one value")
	}
	return nil
}

func exactJSONObject(data []byte, fields []string) (map[string]json.RawMessage, error) {
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("action-route JSON object required")
	}
	objects := make(map[string]json.RawMessage, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok || !allowed[name] {
			return nil, fmt.Errorf("unknown exact action-route field %q", name)
		}
		if _, exists := objects[name]; exists {
			return nil, fmt.Errorf("duplicate action-route field %q", name)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		objects[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("action-route JSON must contain exactly one object")
	}
	return objects, nil
}

func ActionRouteDeclaration(command []string) (manifest.Action, manifest.Interface, error) {
	action := manifest.Action{ID: ActionRouteActionID, Title: "Route plugin actions", Contexts: []string{"global"}, Command: append([]string(nil), command...)}
	contract := manifest.Interface{ID: ActionRouteInterface, Version: ActionRouteInterfaceVersion, Direction: manifest.InterfaceProvides}
	probe := manifest.Manifest{PluginID: "route.probe", Name: "Route probe", Version: "0", Executable: commandAt(command, 0), Actions: []manifest.Action{action}, Interfaces: []manifest.Interface{contract}}
	if err := probe.Validate(); err != nil {
		return manifest.Action{}, manifest.Interface{}, fmt.Errorf("action-route declaration: %w", err)
	}
	return action, contract, nil
}

func commandAt(command []string, index int) string {
	if index >= len(command) {
		return ""
	}
	return command[index]
}
