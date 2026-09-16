package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
)

type RouteProcessCaller struct {
	HerdrRunner    command.Runner
	ProviderRunner command.SpecRunner
	Herdr          string
	Session        string
	Now            func() time.Time
}

func (c RouteProcessCaller) Call(ctx context.Context, request Request) (Response, error) {
	if ctx == nil {
		panic("nil context")
	}
	if c.HerdrRunner == nil || c.ProviderRunner == nil {
		return Response{}, errors.New("action-route process caller requires Herdr and provider runners")
	}
	if c.Session != "" {
		if err := herdrid.ValidateSessionName(c.Session); err != nil {
			return Response{}, err
		}
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if err := validateRouteRequest(ctx, request, now()); err != nil {
		return Response{}, err
	}
	origin, err := routeRequestOrigin(request)
	if err != nil {
		return Response{}, err
	}
	if origin.Session != c.Session {
		return Response{}, errors.New("action-route origin session differs from the configured Herdr session")
	}
	callCtx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	root, argv, err := c.discover(callCtx, request.TargetPlugin)
	if err != nil {
		return Response{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	result, err := c.ProviderRunner.RunSpec(callCtx, command.Spec{Executable: argv[0], Args: append([]string(nil), argv[1:]...), Dir: root, Stdin: encoded})
	if err != nil {
		return Response{}, err
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return Response{}, &CallError{Category: PayloadTooLarge, Message: "action-route provider output exceeded its bound"}
	}
	if len(result.Stdout) == 0 {
		return Response{}, errors.New("action-route provider returned no response")
	}
	var response Response
	if err := decodeRouteEnvelopeResponse(result.Stdout, &response); err != nil {
		return Response{}, fmt.Errorf("decode action-route response: %w", err)
	}
	if err := validateRouteEnvelopeResponse(callCtx, request, response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func (c RouteProcessCaller) discover(ctx context.Context, target string) (string, []string, error) {
	if err := manifest.ValidateID("target_plugin", target); err != nil {
		return "", nil, err
	}
	args := []string{"plugin", "list", "--json"}
	args, err := herdrid.Args(c.Session, args)
	if err != nil {
		return "", nil, err
	}
	result, err := herdrcmd.Run(ctx, c.HerdrRunner, c.Herdr, args)
	if err != nil {
		return "", nil, err
	}
	var envelope routePluginListEnvelope
	if err := decodeHerdrObservation(result.Stdout, &envelope); err != nil {
		return "", nil, fmt.Errorf("decode Herdr plugin list: %w", err)
	}
	if envelope.ID != "cli:plugin" || envelope.Result.Type != "plugin_list" {
		return "", nil, errors.New("unexpected Herdr plugin-list response")
	}
	var match *routeListedPlugin
	for index := range envelope.Result.Plugins {
		plugin := &envelope.Result.Plugins[index]
		if plugin.PluginID != target {
			continue
		}
		if match != nil {
			return "", nil, fmt.Errorf("provider %q is listed more than once", target)
		}
		match = plugin
	}
	if match == nil {
		return "", nil, &CallError{Category: NotFound, Message: "action-route provider is not installed"}
	}
	if !match.Enabled {
		return "", nil, &CallError{Category: Unavailable, Message: "action-route provider is disabled"}
	}
	root := match.PluginRoot
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", nil, errors.New("provider root is not a clean absolute path")
	}
	wantManifest := filepath.Join(root, "herdr-plugin.toml")
	if match.ManifestPath != wantManifest {
		return "", nil, errors.New("provider manifest path drifted from its root")
	}
	kit, herdr, err := readRouteProviderManifests(root)
	if err != nil {
		return "", nil, err
	}
	if kit.PluginID != target {
		return "", nil, errors.New("provider manifest identity changed")
	}
	provided := 0
	for _, contract := range kit.Interfaces {
		if contract.ID == ActionRouteInterface && contract.Version == ActionRouteInterfaceVersion && contract.Direction == manifest.InterfaceProvides {
			provided++
		}
	}
	if provided != 1 {
		return "", nil, errors.New("provider does not advertise exactly one action-route v1 interface")
	}
	var declared *manifest.Action
	for index := range kit.Actions {
		if kit.Actions[index].ID != ActionRouteActionID {
			continue
		}
		if declared != nil {
			return "", nil, errors.New("provider manifest declares the route action more than once")
		}
		declared = &kit.Actions[index]
	}
	if declared == nil {
		return "", nil, errors.New("provider manifest does not declare the route action")
	}
	if !slices.Equal(declared.Contexts, []string{"global"}) {
		return "", nil, errors.New("provider route action must be global")
	}
	var diskAction *routeDiskAction
	for index := range herdr.Actions {
		if herdr.Actions[index].ID != ActionRouteActionID {
			continue
		}
		if diskAction != nil {
			return "", nil, errors.New("Herdr manifest declares the route action more than once")
		}
		diskAction = &herdr.Actions[index]
	}
	if herdr.ID != target || diskAction == nil {
		return "", nil, errors.New("Herdr manifest route identity changed")
	}
	if diskAction.Title != declared.Title || diskAction.Description != declared.Description || !slices.Equal(diskAction.Contexts, declared.Contexts) || !slices.Equal(diskAction.Command, declared.Command) {
		return "", nil, errors.New("route action drifted between Herdr and plugin-kit manifests")
	}
	var discovered *routeListedAction
	for index := range match.Actions {
		if match.Actions[index].ID != ActionRouteActionID {
			continue
		}
		if discovered != nil {
			return "", nil, errors.New("Herdr listed the provider route action more than once")
		}
		discovered = &match.Actions[index]
	}
	if discovered == nil {
		return "", nil, errors.New("Herdr did not list the provider route action")
	}
	description := ""
	if discovered.Description != nil {
		description = *discovered.Description
	}
	if discovered.Title != declared.Title || description != declared.Description || !slices.Equal(discovered.Contexts, declared.Contexts) || !slices.Equal(discovered.Command, declared.Command) {
		return "", nil, errors.New("provider route action drifted between Herdr and plugin-kit manifests")
	}
	return root, append([]string(nil), discovered.Command...), nil
}

func readRouteProviderManifests(root string) (manifest.Manifest, routeDiskManifest, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return manifest.Manifest{}, routeDiskManifest{}, fmt.Errorf("open provider root: %w", err)
	}
	defer directory.Close()
	for _, name := range []string{"herdr-plugin.toml", "plugin-kit.json"} {
		info, err := directory.Lstat(name)
		if err != nil {
			return manifest.Manifest{}, routeDiskManifest{}, fmt.Errorf("inspect provider %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return manifest.Manifest{}, routeDiskManifest{}, fmt.Errorf("provider %s must be a regular file", name)
		}
	}
	kitData, err := readRouteProviderFile(directory, "plugin-kit.json")
	if err != nil {
		return manifest.Manifest{}, routeDiskManifest{}, err
	}
	kit, err := manifest.Parse(bytes.NewReader(kitData))
	if err != nil {
		return manifest.Manifest{}, routeDiskManifest{}, fmt.Errorf("parse provider plugin-kit.json: %w", err)
	}
	herdrData, err := readRouteProviderFile(directory, "herdr-plugin.toml")
	if err != nil {
		return manifest.Manifest{}, routeDiskManifest{}, err
	}
	var herdr routeDiskManifest
	if err := toml.Unmarshal(herdrData, &herdr); err != nil {
		return manifest.Manifest{}, routeDiskManifest{}, fmt.Errorf("parse provider herdr-plugin.toml: %w", err)
	}
	return kit, herdr, nil
}

func readRouteProviderFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open provider %s: %w", name, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, manifest.MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read provider %s: %w", name, err)
	}
	if len(data) > manifest.MaxManifestBytes {
		return nil, fmt.Errorf("provider %s exceeds %d bytes", name, manifest.MaxManifestBytes)
	}
	return data, nil
}

func decodeHerdrObservation(data []byte, target any) error {
	if len(data) > herdrcmd.MaxJSONBytes {
		return errors.New("Herdr plugin list exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("Herdr plugin list must contain exactly one JSON value")
	}
	return nil
}

type routePluginListEnvelope struct {
	ID     string `json:"id"`
	Result struct {
		Type    string              `json:"type"`
		Plugins []routeListedPlugin `json:"plugins"`
	} `json:"result"`
}

type routeListedPlugin struct {
	PluginID     string              `json:"plugin_id"`
	Enabled      bool                `json:"enabled"`
	PluginRoot   string              `json:"plugin_root"`
	ManifestPath string              `json:"manifest_path"`
	Actions      []routeListedAction `json:"actions"`
}

type routeListedAction struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description *string  `json:"description,omitempty"`
	Contexts    []string `json:"contexts"`
	Command     []string `json:"command"`
}

type routeDiskManifest struct {
	ID      string            `toml:"id"`
	Actions []routeDiskAction `toml:"actions"`
}

type routeDiskAction struct {
	ID          string   `toml:"id"`
	Title       string   `toml:"title"`
	Description string   `toml:"description,omitempty"`
	Contexts    []string `toml:"contexts"`
	Command     []string `toml:"command"`
}
