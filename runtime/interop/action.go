package interop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/actionhost"
)

const MaxActionEnvelopeBytes = 64 << 10

// ActionCapabilities describes the inter-plugin transport Herdr 0.8.2 exposes.
// Request envelopes and caller-selected correlation IDs are deliberately false:
// action invocation carries only a declared plugin and action identity.
type ActionCapabilities struct {
	Discovery          bool
	Invocation         bool
	ReceiptCorrelation bool
	ResponseEnvelope   bool
	RequestEnvelope    bool
	CallerCorrelation  bool
}

func SupportedActionCapabilities() ActionCapabilities {
	return ActionCapabilities{
		Discovery: true, Invocation: true, ReceiptCorrelation: true, ResponseEnvelope: true,
	}
}

// ActionTarget binds a static Herdr action to one versioned no-input method.
type ActionTarget struct {
	PluginID         string
	ActionID         string
	Interface        string
	InterfaceVersion uint32
	Method           string
}

// ActionResponse is the bounded JSON value emitted on successful action stdout.
// The Herdr receipt log ID, returned separately in ActionResult, is the call's
// correlation identity.
type ActionResponse struct {
	Version          uint32          `json:"version"`
	Interface        string          `json:"interface"`
	InterfaceVersion uint32          `json:"interface_version"`
	Method           string          `json:"method"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Error            *CallError      `json:"error,omitempty"`
}

type ActionResult struct {
	LogID    string
	Response ActionResponse
}

type ActionTransport interface {
	List(context.Context, string) ([]actionhost.Action, error)
	Invoke(context.Context, string, string) (actionhost.Receipt, error)
	AwaitReceipt(context.Context, actionhost.Receipt, time.Duration) (actionhost.Receipt, error)
}

type ActionClient struct {
	Host         ActionTransport
	PollInterval time.Duration
}

func (c ActionClient) Discover(ctx context.Context, target ActionTarget) (actionhost.Action, error) {
	if err := validateActionTarget(ctx, target); err != nil {
		return actionhost.Action{}, err
	}
	if c.Host == nil {
		return actionhost.Action{}, errors.New("action interoperability requires an ActionHost")
	}
	actions, err := c.Host.List(ctx, target.PluginID)
	if err != nil {
		return actionhost.Action{}, err
	}
	var match actionhost.Action
	found := false
	for _, action := range actions {
		if action.PluginID != target.PluginID || action.ActionID != target.ActionID {
			continue
		}
		if found {
			return actionhost.Action{}, fmt.Errorf("action %q is declared more than once", target.ActionID)
		}
		match, found = action, true
	}
	if !found {
		return actionhost.Action{}, &CallError{Category: NotFound, Message: "plugin action is not available"}
	}
	return match, nil
}

func (c ActionClient) Invoke(ctx context.Context, target ActionTarget) (ActionResult, error) {
	if c.PollInterval <= 0 {
		return ActionResult{}, errors.New("poll interval must be positive")
	}
	if _, err := c.Discover(ctx, target); err != nil {
		return ActionResult{}, err
	}
	initial, err := c.Host.Invoke(ctx, target.PluginID, target.ActionID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := validateActionReceipt(initial, target, initial.LogID); err != nil {
		return ActionResult{}, err
	}
	receipt, err := c.Host.AwaitReceipt(ctx, initial, c.PollInterval)
	if err != nil {
		return ActionResult{}, err
	}
	if err := validateActionReceipt(receipt, target, initial.LogID); err != nil {
		return ActionResult{}, err
	}
	if receipt.Status != actionhost.Succeeded {
		return ActionResult{}, &CallError{Category: Internal, Message: "plugin action failed"}
	}
	if receipt.Stdout == nil || strings.TrimSpace(*receipt.Stdout) == "" {
		return ActionResult{}, &CallError{Category: InvalidRequest, Message: "plugin action returned no response envelope"}
	}
	response, err := DecodeActionResponse(strings.NewReader(*receipt.Stdout), target)
	if err != nil {
		return ActionResult{}, err
	}
	return ActionResult{LogID: receipt.LogID, Response: response}, nil
}

func validateActionReceipt(receipt actionhost.Receipt, target ActionTarget, logID string) error {
	if logID == "" || receipt.LogID != logID {
		return errors.New("action receipt identity changed")
	}
	if receipt.PluginID != target.PluginID || receipt.ActionID == nil || *receipt.ActionID != target.ActionID {
		return errors.New("action receipt identity changed")
	}
	return nil
}

func DecodeActionResponse(reader io.Reader, target ActionTarget) (ActionResponse, error) {
	if err := validateActionTarget(context.Background(), target); err != nil {
		return ActionResponse{}, err
	}
	var response ActionResponse
	if err := decodeStrictLimit(reader, &response, MaxActionEnvelopeBytes); err != nil {
		return ActionResponse{}, err
	}
	if err := validateActionResponse(response, target); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func validateActionTarget(ctx context.Context, target ActionTarget) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := manifest.ValidateID("plugin_id", target.PluginID); err != nil {
		return err
	}
	if err := manifest.ValidateID("action_id", target.ActionID); err != nil {
		return err
	}
	if err := manifest.ValidateID("interface", target.Interface); err != nil {
		return err
	}
	if target.InterfaceVersion == 0 {
		return errors.New("interface_version must be positive")
	}
	return manifest.ValidateID("method", target.Method)
}

func validateActionResponse(response ActionResponse, target ActionTarget) error {
	if response.Version != Version {
		return fmt.Errorf("unsupported action response version %d", response.Version)
	}
	if response.Interface != target.Interface || response.InterfaceVersion != target.InterfaceVersion || response.Method != target.Method {
		return errors.New("action response identity changed")
	}
	if response.Error != nil && len(response.Payload) != 0 {
		return errors.New("action response cannot contain both payload and error")
	}
	if response.Error != nil && !validCallError(response.Error) {
		return errors.New("action response has invalid typed error")
	}
	return validatePayload(response.Payload)
}
