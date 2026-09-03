package catalogue_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/internal/catalogue"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type golden struct {
	Viewports []struct {
		ID           string `json:"id"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		KeyboardOpen bool   `json:"keyboard_open"`
	} `json:"viewports"`
	Scenarios         []string          `json:"scenarios"`
	ThemeCount        int               `json:"theme_count"`
	Entries           int               `json:"entry_count"`
	Sheets            int               `json:"contact_sheet_count"`
	MatrixFrameHashes map[string]string `json:"matrix_frame_hashes"`
}

func loadGolden(t *testing.T) (string, golden) {
	t.Helper()
	path := filepath.Join("..", "..", "catalogue", "testdata", "matrix.golden.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want golden
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	return path, want
}

func TestFinalMatrixGolden(t *testing.T) {
	_, want := loadGolden(t)
	viewports := catalogue.Viewports()
	if len(viewports) != len(want.Viewports) {
		t.Fatalf("viewports = %d", len(viewports))
	}
	for index, viewport := range viewports {
		if viewport.ID != want.Viewports[index].ID || viewport.Width != want.Viewports[index].Width || viewport.Height != want.Viewports[index].Height || viewport.KeyboardOpen != want.Viewports[index].KeyboardOpen {
			t.Fatalf("viewport %d = %+v", index, viewport)
		}
	}
	states, surfaces := map[string]bool{}, map[string]bool{}
	gotScenarios := make([]string, 0, len(catalogue.Scenarios()))
	for _, scenario := range catalogue.Scenarios() {
		gotScenarios = append(gotScenarios, scenario.ID)
		states[scenario.State] = true
		for _, surface := range scenario.Surfaces {
			surfaces[surface] = true
		}
	}
	if !reflect.DeepEqual(gotScenarios, want.Scenarios) {
		t.Fatalf("scenarios = %v", gotScenarios)
	}
	for _, surface := range []string{"recall-search", "attention-switcher", "session-switcher", "plugin-configurator", "action-finder", "debug-ui"} {
		if !surfaces[surface] {
			t.Errorf("surface %s absent", surface)
		}
	}
	for _, state := range []string{"results-selected", "validation-error", "live-applied", "timeline-detail", "storage-health", "screenshot", "loading", "empty", "error", "long-content"} {
		if !states[state] {
			t.Errorf("state %s absent", state)
		}
	}
	specs, err := catalogue.Matrix(catalogue.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(theme.IDs()) != want.ThemeCount || len(specs) != want.Entries {
		t.Fatalf("themes/specs = %d/%d", len(theme.IDs()), len(specs))
	}
	if specs[0].RelativePath() != "bento-command/catppuccin/minimum/picker-search-selected.png" || specs[len(specs)-1].RelativePath() != "bento-command/vesper/maximum/long-content.png" {
		t.Fatalf("matrix endpoints = %q .. %q", specs[0].RelativePath(), specs[len(specs)-1].RelativePath())
	}
}

func TestFinalMatrixFramesMatchReviewedVisualContract(t *testing.T) {
	path, want := loadGolden(t)
	got := matrixFrameHashes(t)
	if os.Getenv("HERDR_UPDATE_CATALOGUE_GOLDEN") == "1" {
		want.MatrixFrameHashes = got
		encoded, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !reflect.DeepEqual(got, want.MatrixFrameHashes) {
		gotKeys, wantKeys := sortedKeys(got), sortedKeys(want.MatrixFrameHashes)
		if !reflect.DeepEqual(gotKeys, wantKeys) {
			t.Fatalf("visual contract keys = %v, want %v", gotKeys, wantKeys)
		}
		for _, key := range gotKeys {
			if got[key] != want.MatrixFrameHashes[key] {
				t.Errorf("%s visual contract = %s, want %s", key, got[key], want.MatrixFrameHashes[key])
			}
		}
	}
}

func matrixFrameHashes(t *testing.T) map[string]string {
	t.Helper()
	specs, err := catalogue.Matrix(catalogue.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	digests := make(map[string]hash.Hash)
	for _, spec := range specs {
		key := spec.ThemeID + "/" + spec.Viewport.ID
		digest := digests[key]
		if digest == nil {
			digest = sha256.New()
			digest.Write([]byte("herdr-catalogue-bento-command-v1\x00"))
			digests[key] = digest
		}
		frame, err := catalogue.Render(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec.RelativePath(), err)
		}
		writeString(digest, spec.RelativePath())
		writeInteger(digest, frame.Width())
		writeInteger(digest, frame.Height())
		for y := 0; y < frame.Height(); y++ {
			for x := 0; x < frame.Width(); x++ {
				cell, ok := frame.CellAt(x, y)
				if !ok {
					t.Fatalf("%s missing cell at %d,%d", spec.RelativePath(), x, y)
				}
				writeString(digest, cell.Text)
				writeString(digest, string(cell.Style.Foreground))
				writeString(digest, string(cell.Style.Background))
				writeInteger(digest, cell.Width)
				writeBool(digest, cell.Continuation)
				writeBool(digest, cell.Style.Bold)
				writeBool(digest, cell.Style.Dim)
				writeBool(digest, cell.Style.Underline)
			}
		}
	}
	result := make(map[string]string, len(digests))
	for key, digest := range digests {
		result[key] = hex.EncodeToString(digest.Sum(nil))
	}
	return result
}

func writeInteger(digest hash.Hash, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	digest.Write(encoded[:])
}

func writeString(digest hash.Hash, value string) {
	writeInteger(digest, len(value))
	digest.Write([]byte(value))
}

func writeBool(digest hash.Hash, value bool) {
	if value {
		digest.Write([]byte{1})
		return
	}
	digest.Write([]byte{0})
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestFinalCommandRendersEveryThemeViewportAndScenario(t *testing.T) {
	for _, viewport := range catalogue.Viewports() {
		for _, scenario := range catalogue.Scenarios() {
			frame, err := catalogue.Render(catalogue.Spec{ThemeID: "catppuccin", Viewport: viewport, Scenario: scenario})
			if err != nil {
				t.Fatalf("%s/%s: %v", viewport.ID, scenario.ID, err)
			}
			if frame.Width() != viewport.Width || frame.Height() != viewport.Height {
				t.Fatalf("%s/%s dimensions", viewport.ID, scenario.ID)
			}
			for _, point := range [][2]int{{0, 0}, {frame.Width() - 1, frame.Height() - 1}} {
				cell, ok := frame.CellAt(point[0], point[1])
				if !ok || cell.Style.Background == "" {
					t.Fatalf("%s/%s edge %v", viewport.ID, scenario.ID, point)
				}
			}
		}
	}
	for _, themeID := range theme.IDs() {
		if _, err := catalogue.Render(catalogue.Spec{ThemeID: themeID, Viewport: catalogue.Viewports()[0], Scenario: catalogue.Scenarios()[0]}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportHonorsThemeViewportAndScenarioFilters(t *testing.T) {
	output := filepath.Join(t.TempDir(), "catalogue")
	manifest, err := catalogue.Export(context.Background(), output, catalogue.Selection{ThemeIDs: []string{"nord"}, ViewportIDs: []string{"phone-keyboard"}, ScenarioIDs: []string{"error"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || len(manifest.ContactSheets) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Entries[0].Path != "bento-command/nord/phone-keyboard/error.png" {
		t.Fatal(manifest.Entries[0].Path)
	}
	if _, err := os.Stat(filepath.Join(output, manifest.Entries[0].Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, manifest.ContactSheets[0].Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Matrix(catalogue.Selection{ThemeIDs: []string{"unknown"}}); err == nil || !strings.Contains(err.Error(), "unknown theme") {
		t.Fatalf("filter error = %v", err)
	}
	_ = view.PNGCellWidth
}
