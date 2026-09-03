package catalogue

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

const (
	manifestVersion = 2
	designID        = "bento-command"
)

type Viewport struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Scenario struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	State    string   `json:"state"`
	Surfaces []string `json:"surfaces"`
}

type Spec struct {
	ThemeID  string
	Viewport Viewport
	Scenario Scenario
}

func (s Spec) RelativePath() string {
	return filepath.ToSlash(filepath.Join(designID, s.ThemeID, s.Viewport.ID, s.Scenario.ID+".png"))
}

type Selection struct {
	ThemeIDs    []string
	ViewportIDs []string
	ScenarioIDs []string
}

var viewports = []Viewport{
	{ID: "minimum", Name: "Minimum", Width: 40, Height: 10},
	{ID: "phone-keyboard", Name: "Phone + keyboard", Width: 48, Height: 18},
	{ID: "phone", Name: "Phone", Width: 48, Height: 30},
	{ID: "landscape-recovery", Name: "Landscape + keyboard", Width: 78, Height: 10},
	{ID: "landscape", Name: "Landscape", Width: 78, Height: 20},
	{ID: "standard", Name: "Standard", Width: 80, Height: 18},
	{ID: "wide", Name: "Wide", Width: 110, Height: 24},
	{ID: "maximum", Name: "Maximum", Width: 500, Height: 200},
}

var scenarios = []Scenario{
	{ID: "picker-search-selected", Name: "Search with selected result", State: "results-selected", Surfaces: []string{"recall-search", "attention-switcher", "session-switcher", "action-finder"}},
	{ID: "config-validation", Name: "Configuration validation", State: "validation-error", Surfaces: []string{"plugin-configurator"}},
	{ID: "config-live-applied", Name: "Configuration live applied", State: "live-applied", Surfaces: []string{"plugin-configurator"}},
	{ID: "diagnostic-timeline-detail", Name: "Event timeline and detail", State: "timeline-detail", Surfaces: []string{"debug-ui"}},
	{ID: "diagnostic-storage-health", Name: "Storage health", State: "storage-health", Surfaces: []string{"debug-ui"}},
	{ID: "diagnostic-screenshot", Name: "Screenshot inspection", State: "screenshot", Surfaces: []string{"debug-ui"}},
	{ID: "loading", Name: "Loading", State: "loading", Surfaces: []string{"recall-search", "attention-switcher", "session-switcher", "plugin-configurator", "action-finder", "debug-ui"}},
	{ID: "empty", Name: "Empty", State: "empty", Surfaces: []string{"recall-search", "attention-switcher", "session-switcher", "action-finder", "debug-ui"}},
	{ID: "error", Name: "Recoverable error", State: "error", Surfaces: []string{"recall-search", "plugin-configurator", "debug-ui"}},
	{ID: "long-content", Name: "Long content", State: "long-content", Surfaces: []string{"recall-search", "session-switcher", "plugin-configurator", "debug-ui"}},
}

func Viewports() []Viewport { return slices.Clone(viewports) }

func Scenarios() []Scenario {
	result := slices.Clone(scenarios)
	for i := range result {
		result[i].Surfaces = slices.Clone(result[i].Surfaces)
	}
	return result
}

func Matrix(selection Selection) ([]Spec, error) {
	selectedThemes, err := selectValues(theme.IDs(), selection.ThemeIDs, func(v string) string { return v }, "theme")
	if err != nil {
		return nil, err
	}
	selectedViewports, err := selectValues(viewports, selection.ViewportIDs, func(v Viewport) string { return v.ID }, "viewport")
	if err != nil {
		return nil, err
	}
	selectedScenarios, err := selectValues(scenarios, selection.ScenarioIDs, func(v Scenario) string { return v.ID }, "scenario")
	if err != nil {
		return nil, err
	}
	result := make([]Spec, 0, len(selectedThemes)*len(selectedViewports)*len(selectedScenarios))
	for _, themeID := range selectedThemes {
		for _, viewport := range selectedViewports {
			for _, scenario := range selectedScenarios {
				result = append(result, Spec{ThemeID: themeID, Viewport: viewport, Scenario: scenario})
			}
		}
	}
	return result, nil
}

func selectValues[T any](available []T, requested []string, id func(T) string, kind string) ([]T, error) {
	if len(requested) == 0 {
		return slices.Clone(available), nil
	}
	wanted := make(map[string]bool, len(requested))
	for _, value := range requested {
		if value == "" || filepath.Base(value) != value || value == "." || value == ".." {
			return nil, fmt.Errorf("unsafe %s ID %q", kind, value)
		}
		wanted[value] = true
	}
	result := make([]T, 0, len(wanted))
	for _, value := range available {
		if wanted[id(value)] {
			result = append(result, value)
			delete(wanted, id(value))
		}
	}
	for value := range wanted {
		return nil, fmt.Errorf("unknown %s %q", kind, value)
	}
	return result, nil
}
