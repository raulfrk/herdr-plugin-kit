// Package actionhost wraps the action contract supported by Herdr 0.8.2.
package actionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
)

const MaxJSONBytes = herdrcmd.MaxJSONBytes

type Host struct {
	Runner command.Runner
	Herdr  string
}

type Action struct {
	PluginID    string   `json:"plugin_id"`
	ActionID    string   `json:"action_id"`
	Title       string   `json:"title"`
	Command     []string `json:"command"`
	Contexts    []string `json:"contexts,omitempty"`
	Description *string  `json:"description,omitempty"`
	Platforms   []string `json:"platforms,omitempty"`
}

type Status string

const (
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

type Receipt struct {
	LogID          string   `json:"log_id"`
	PluginID       string   `json:"plugin_id"`
	ActionID       *string  `json:"action_id"`
	Command        []string `json:"command"`
	Status         Status   `json:"status"`
	StartedUnixMS  uint64   `json:"started_unix_ms"`
	FinishedUnixMS *uint64  `json:"finished_unix_ms"`
	ExitCode       *int     `json:"exit_code"`
	Stdout         *string  `json:"stdout"`
	Stderr         *string  `json:"stderr"`
	Error          *string  `json:"error"`
	Event          *string  `json:"event"`
}

func (r Receipt) Terminal() bool { return r.Status == Succeeded || r.Status == Failed }

func (h Host) List(ctx context.Context, pluginID string) ([]Action, error) {
	if pluginID != "" {
		if err := manifest.ValidateID("plugin_id", pluginID); err != nil {
			return nil, err
		}
	}
	args := []string{"plugin", "action", "list"}
	if pluginID != "" {
		args = append(args, "--plugin", pluginID)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type    string   `json:"type"`
			Actions []Action `json:"actions"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, args, &response); err != nil {
		return nil, err
	}
	if response.Result.Type != "plugin_action_list" {
		return nil, fmt.Errorf("unexpected action list result %q", response.Result.Type)
	}
	for i, action := range response.Result.Actions {
		if err := validateAction(action); err != nil {
			return nil, fmt.Errorf("actions[%d]: %w", i, err)
		}
		if pluginID != "" && action.PluginID != pluginID {
			return nil, fmt.Errorf("actions[%d]: plugin identity changed", i)
		}
	}
	return response.Result.Actions, nil
}

func (h Host) Invoke(ctx context.Context, pluginID, actionID string) (Receipt, error) {
	if err := manifest.ValidateID("plugin_id", pluginID); err != nil {
		return Receipt{}, err
	}
	if err := manifest.ValidateID("action_id", actionID); err != nil {
		return Receipt{}, err
	}
	var response invokeResponse
	args := []string{"plugin", "action", "invoke", actionID, "--plugin", pluginID}
	if err := h.runJSON(ctx, args, &response); err != nil {
		return Receipt{}, err
	}
	if response.Result.Type != "plugin_action_invoked" {
		return Receipt{}, fmt.Errorf("unexpected invoke result %q", response.Result.Type)
	}
	if err := validateAction(response.Result.Action); err != nil {
		return Receipt{}, fmt.Errorf("invoked action: %w", err)
	}
	if response.Result.Action.PluginID != pluginID || response.Result.Action.ActionID != actionID {
		return Receipt{}, errors.New("invoked action identity changed")
	}
	if err := validateReceipt(response.Result.Log, pluginID, actionID); err != nil {
		return Receipt{}, err
	}
	return response.Result.Log, nil
}

// Receipt polls the command log once and returns the exact matching log entry.
func (h Host) Receipt(ctx context.Context, pluginID, actionID, logID string) (Receipt, error) {
	if err := manifest.ValidateID("plugin_id", pluginID); err != nil {
		return Receipt{}, err
	}
	if err := manifest.ValidateID("action_id", actionID); err != nil {
		return Receipt{}, err
	}
	if err := validateLogID(logID); err != nil {
		return Receipt{}, err
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string    `json:"type"`
			Logs []Receipt `json:"logs"`
		} `json:"result"`
	}
	if err := h.runJSON(ctx, []string{"plugin", "log", "list", "--plugin", pluginID, "--limit", "100"}, &response); err != nil {
		return Receipt{}, err
	}
	if response.Result.Type != "plugin_log_list" {
		return Receipt{}, fmt.Errorf("unexpected log list result %q", response.Result.Type)
	}
	for _, receipt := range response.Result.Logs {
		if receipt.LogID != logID {
			continue
		}
		if err := validateReceipt(receipt, pluginID, actionID); err != nil {
			return Receipt{}, err
		}
		return receipt, nil
	}
	return Receipt{}, fmt.Errorf("receipt %q not found", logID)
}

func (h Host) AwaitReceipt(ctx context.Context, initial Receipt, interval time.Duration) (Receipt, error) {
	if interval <= 0 {
		return Receipt{}, errors.New("poll interval must be positive")
	}
	if initial.ActionID == nil {
		return Receipt{}, errors.New("receipt has no action_id")
	}
	if err := validateReceipt(initial, initial.PluginID, *initial.ActionID); err != nil {
		return Receipt{}, err
	}
	current := initial
	for !current.Terminal() {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return Receipt{}, ctx.Err()
		case <-timer.C:
		}
		next, err := h.Receipt(ctx, initial.PluginID, *initial.ActionID, initial.LogID)
		if err != nil {
			return Receipt{}, err
		}
		current = next
	}
	return current, nil
}

func (h Host) runJSON(ctx context.Context, args []string, target any) error {
	return herdrcmd.RunJSON(ctx, h.Runner, h.Herdr, args, target)
}

type invokeResponse struct {
	ID     string `json:"id"`
	Result struct {
		Type    string            `json:"type"`
		Action  Action            `json:"action"`
		Context invocationContext `json:"context"`
		Log     Receipt           `json:"log"`
	} `json:"result"`
}

type invocationContext struct {
	WorkspaceID       *string         `json:"workspace_id"`
	WorkspaceLabel    *string         `json:"workspace_label"`
	WorkspaceCWD      *string         `json:"workspace_cwd"`
	Worktree          json.RawMessage `json:"worktree"`
	TabID             *string         `json:"tab_id"`
	TabLabel          *string         `json:"tab_label"`
	FocusedPaneID     *string         `json:"focused_pane_id"`
	FocusedPaneAgent  *string         `json:"focused_pane_agent"`
	FocusedPaneStatus *string         `json:"focused_pane_status"`
	FocusedPaneCWD    *string         `json:"focused_pane_cwd"`
	SelectedText      *string         `json:"selected_text"`
	InvocationSource  *string         `json:"invocation_source"`
	CorrelationID     *string         `json:"correlation_id"`
	ClickedURL        *string         `json:"clicked_url"`
	LinkHandlerID     *string         `json:"link_handler_id"`
}

func validateAction(action Action) error {
	if err := manifest.ValidateID("plugin_id", action.PluginID); err != nil {
		return err
	}
	if err := manifest.ValidateID("action_id", action.ActionID); err != nil {
		return err
	}
	if action.Title == "" || len(action.Title) > 128 {
		return errors.New("invalid action title")
	}
	if len(action.Command) == 0 || len(action.Command) > 128 {
		return errors.New("invalid action command")
	}
	return nil
}

func validateReceipt(receipt Receipt, pluginID, actionID string) error {
	if err := validateLogID(receipt.LogID); err != nil {
		return err
	}
	if receipt.PluginID != pluginID {
		return errors.New("receipt plugin identity changed")
	}
	if receipt.ActionID == nil || *receipt.ActionID != actionID {
		return errors.New("receipt action identity changed")
	}
	if receipt.Status != Running && receipt.Status != Succeeded && receipt.Status != Failed {
		return fmt.Errorf("invalid receipt status %q", receipt.Status)
	}
	if len(receipt.Command) == 0 || len(receipt.Command) > 128 {
		return errors.New("invalid receipt command")
	}
	return nil
}

func validateLogID(value string) error {
	if value == "" || len(value) > 128 || strings.ContainsRune(value, 0) {
		return errors.New("log_id must be 1-128 NUL-free bytes")
	}
	return nil
}
