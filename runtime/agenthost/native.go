package agenthost

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
)

const (
	OMPStatus                     Reason = "omp_status"
	OpenCodeStatus                Reason = "opencode_status"
	OpenCodeQuestion              Reason = "opencode_question"
	OpenCodePermission            Reason = "opencode_permission"
	OpenCodeQuestionAndPermission Reason = "opencode_question_and_permission"
	NativeNotManaged              Reason = "native_not_managed"
	NativeUnavailable             Reason = "native_unavailable"
	NativeUnsupported             Reason = "native_unsupported_status"
)

//go:embed integrations/omp-status.ts
var ompStatusExtension []byte

// OMPStatusExtension returns a fresh copy of the reviewed OMP extension.
func OMPStatusExtension() []byte { return append([]byte(nil), ompStatusExtension...) }

type OpenCodeServer struct {
	URL       string
	Directory string
}

type NativeSources struct {
	OMPSockets      map[string]string
	OpenCodeServers map[string]OpenCodeServer
}

func NewProbeWithNative(host Host, sources NativeSources) (Probe, error) {
	cloned, err := cloneNativeSources(sources)
	if err != nil {
		return Probe{}, err
	}
	return Probe{Host: host, native: cloned, wait: wait}, nil
}

func cloneNativeSources(sources NativeSources) (NativeSources, error) {
	cloned := NativeSources{
		OMPSockets:      make(map[string]string, len(sources.OMPSockets)),
		OpenCodeServers: make(map[string]OpenCodeServer, len(sources.OpenCodeServers)),
	}
	seen := make(map[string]struct{}, len(sources.OMPSockets)+len(sources.OpenCodeServers))
	for pane, socket := range sources.OMPSockets {
		if err := validateBounded("native pane_id", pane, maxIdentityBytes, false); err != nil {
			return NativeSources{}, err
		}
		if _, duplicate := seen[pane]; duplicate {
			return NativeSources{}, fmt.Errorf("duplicate native pane_id %q", pane)
		}
		if !filepath.IsAbs(socket) || strings.ContainsRune(socket, 0) {
			return NativeSources{}, fmt.Errorf("OMP socket for pane %q is invalid", pane)
		}
		seen[pane] = struct{}{}
		cloned.OMPSockets[pane] = socket
	}
	for pane, server := range sources.OpenCodeServers {
		if err := validateBounded("native pane_id", pane, maxIdentityBytes, false); err != nil {
			return NativeSources{}, err
		}
		if _, duplicate := seen[pane]; duplicate {
			return NativeSources{}, fmt.Errorf("duplicate native pane_id %q", pane)
		}
		if err := validateOpenCodeServer(server); err != nil {
			return NativeSources{}, fmt.Errorf("OpenCode server for pane %q: %w", pane, err)
		}
		seen[pane] = struct{}{}
		cloned.OpenCodeServers[pane] = server
	}
	return cloned, nil
}

func validateOpenCodeServer(server OpenCodeServer) error {
	u, err := url.Parse(server.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || strings.ContainsAny(server.URL, "?#") || u.RawPath != "" || u.Path != "" && u.Path != "/" {
		return errors.New("URL is not a plain loopback HTTP origin")
	}
	host, port, err := net.SplitHostPort(u.Host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 || host == "" {
		return errors.New("URL must contain an explicit loopback host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() && (host == "::1" || strings.HasPrefix(host, "127."))) {
		return errors.New("URL host must be a literal loopback address")
	}
	if !filepath.IsAbs(server.Directory) || strings.ContainsRune(server.Directory, 0) {
		return errors.New("directory must be an absolute NUL-free path")
	}
	return nil
}

func nativeUnknown(reason Reason) Assessment { return Assessment{Status: Unknown, Reason: reason} }

func nativeFailure(err error) (Assessment, error) {
	if errors.Is(err, ErrStaleReport) {
		return nativeUnknown(Unsettled), ErrStaleReport
	}
	if err == nil {
		err = errors.New("native status protocol unavailable")
	}
	return nativeUnknown(NativeUnavailable), err
}

func nativeAssessmentFailure(ctx context.Context, harness string, err error) (Assessment, error) {
	if errors.Is(err, ErrStaleReport) {
		return nativeFailure(ErrStaleReport)
	}
	if ctx.Err() != nil {
		return nativeFailure(ctx.Err())
	}
	return nativeFailure(fmt.Errorf("assess %s: native status unavailable", harness))
}

func validateNativeTarget(host Host, target Agent, kind, source, sessionKind string) (bool, error) {
	for name, value := range map[string]string{
		"agent kind": target.Kind, "workspace_id": target.WorkspaceID,
		"tab_id": target.TabID, "pane_id": target.PaneID,
	} {
		if err := validateBounded(name, value, maxIdentityBytes, false); err != nil {
			return false, err
		}
	}
	if !strings.EqualFold(target.Kind, kind) {
		return false, errors.New("native target kind is invalid")
	}
	if host.Session == "" || target.SessionName == "" {
		return false, nil
	}
	if err := herdrid.ValidateSessionName(host.Session); err != nil {
		return false, err
	}
	if err := herdrid.ValidateSessionName(target.SessionName); err != nil {
		return false, err
	}
	if target.SessionName != host.Session || target.Session == nil {
		return false, nil
	}
	if err := validateSession(*target.Session); err != nil {
		return false, err
	}
	s := target.Session
	return s.Source == source && s.Agent == kind && s.Kind == sessionKind, nil
}

type nativeIdentity struct {
	Kind                string
	WorkspaceID, TabID  string
	PaneID, SessionName string
	Session             SessionRef
}

func nativeIdentityFromAgent(a Agent) nativeIdentity {
	identity := nativeIdentity{Kind: a.Kind, WorkspaceID: a.WorkspaceID, TabID: a.TabID, PaneID: a.PaneID, SessionName: a.SessionName}
	if a.Session != nil {
		identity.Session = *a.Session
	}
	return identity
}

func sameNativeIdentity(a, b nativeIdentity) bool {
	return strings.EqualFold(a.Kind, b.Kind) &&
		a.WorkspaceID == b.WorkspaceID && a.TabID == b.TabID && a.PaneID == b.PaneID &&
		a.SessionName == b.SessionName && a.Session == b.Session
}

func (h Host) findNativeIdentity(ctx context.Context, pane string) (nativeIdentity, error) {
	args, err := h.args([]string{"agent", "list"})
	if err != nil {
		return nativeIdentity{}, err
	}
	result, err := herdrcmd.Run(ctx, h.Runner, h.Herdr, args)
	if err != nil {
		return nativeIdentity{}, err
	}
	if len(result.Stdout) > herdrcmd.MaxJSONBytes {
		return nativeIdentity{}, errors.New("Herdr JSON exceeds size limit")
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := decodeStrictOne(result.Stdout, &envelope); err != nil {
		return nativeIdentity{}, fmt.Errorf("decode Herdr identity envelope: %w", err)
	}
	var list struct {
		Type   string            `json:"type"`
		Agents []json.RawMessage `json:"agents"`
	}
	if err := decodeStrictOne(envelope.Result, &list); err != nil || list.Type != "agent_list" {
		return nativeIdentity{}, errors.New("decode Herdr identity result: invalid agent list")
	}
	var found *nativeIdentity
	seen := make(map[string]struct{}, len(list.Agents))
	for i, raw := range list.Agents {
		var wire struct {
			Agent        string       `json:"agent"`
			AgentSession *wireSession `json:"agent_session"`
			WorkspaceID  string       `json:"workspace_id"`
			TabID        string       `json:"tab_id"`
			PaneID       string       `json:"pane_id"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nativeIdentity{}, fmt.Errorf("decode Herdr identity agents[%d]", i)
		}
		for name, value := range map[string]string{"agent": wire.Agent, "workspace_id": wire.WorkspaceID, "tab_id": wire.TabID, "pane_id": wire.PaneID} {
			if err := validateBounded(name, value, maxIdentityBytes, false); err != nil {
				return nativeIdentity{}, fmt.Errorf("Herdr identity agents[%d]: %w", i, err)
			}
		}
		if _, duplicate := seen[wire.PaneID]; duplicate {
			return nativeIdentity{}, errors.New("Herdr identity contains duplicate pane_id")
		}
		seen[wire.PaneID] = struct{}{}
		if wire.PaneID != pane {
			continue
		}
		if wire.AgentSession == nil {
			return nativeIdentity{}, ErrStaleReport
		}
		session := SessionRef{Source: wire.AgentSession.Source, Agent: wire.AgentSession.Agent, Kind: wire.AgentSession.Kind, Value: wire.AgentSession.Value}
		if err := validateSession(session); err != nil {
			return nativeIdentity{}, ErrStaleReport
		}
		identity := nativeIdentity{Kind: wire.Agent, WorkspaceID: wire.WorkspaceID, TabID: wire.TabID, PaneID: wire.PaneID, SessionName: h.Session, Session: session}
		found = &identity
	}
	if found == nil {
		return nativeIdentity{}, ErrStaleReport
	}
	return *found, nil
}

func decodeStrictOne(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}
