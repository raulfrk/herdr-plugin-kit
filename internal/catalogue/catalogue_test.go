package catalogue_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/internal/catalogue"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type matrixFixture struct {
	Designs   []string `json:"designs"`
	Viewports []struct {
		ID           string `json:"id"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		KeyboardOpen bool   `json:"keyboard_open"`
	} `json:"viewports"`
	Scenarios          []string          `json:"scenarios"`
	ThemeCount         int               `json:"theme_count"`
	EntryCount         int               `json:"entry_count"`
	ContactSheetCount  int               `json:"contact_sheet_count"`
	MatrixFrameHashes  map[string]string `json:"matrix_frame_hashes"`
	RenderHashes       map[string]string `json:"render_hashes"`
	ContactSheetHashes map[string]string `json:"contact_sheet_hashes"`
}

func loadFixture(t *testing.T) matrixFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "catalogue", "testdata", "matrix.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture matrixFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestHYPCAT001MatrixIsCompleteOrderedAndReviewable(t *testing.T) {
	fixture := loadFixture(t)
	designs := catalogue.Designs()
	gotDesigns := make([]string, len(designs))
	for i, design := range designs {
		gotDesigns[i] = design.ID
	}
	if !reflect.DeepEqual(gotDesigns, fixture.Designs) {
		t.Fatalf("design IDs = %v, want %v", gotDesigns, fixture.Designs)
	}
	if len(designs) < 3 || designs[0].LayoutSignature == designs[1].LayoutSignature || designs[0].LayoutSignature == designs[2].LayoutSignature || designs[1].LayoutSignature == designs[2].LayoutSignature {
		t.Fatalf("design families are not structurally distinct: %+v", designs)
	}
	viewports := catalogue.Viewports()
	if len(viewports) != len(fixture.Viewports) {
		t.Fatalf("viewport count = %d", len(viewports))
	}
	for i, want := range fixture.Viewports {
		got := viewports[i]
		if got.ID != want.ID || got.Width != want.Width || got.Height != want.Height || got.KeyboardOpen != want.KeyboardOpen {
			t.Fatalf("viewport %d = %+v, want %+v", i, got, want)
		}
	}
	scenarios := catalogue.Scenarios()
	gotScenarios := make([]string, len(scenarios))
	coveredSurfaces, coveredStates := map[string]bool{}, map[string]bool{}
	for i, scenario := range scenarios {
		gotScenarios[i] = scenario.ID
		for _, surface := range scenario.Surfaces {
			coveredSurfaces[surface] = true
		}
		coveredStates[scenario.State] = true
	}
	if !reflect.DeepEqual(gotScenarios, fixture.Scenarios) {
		t.Fatalf("scenario IDs = %v", gotScenarios)
	}
	for _, surface := range []string{"recall-search", "attention-switcher", "session-switcher", "plugin-configurator", "action-finder", "debug-ui"} {
		if !coveredSurfaces[surface] {
			t.Errorf("surface %q is uncovered", surface)
		}
	}
	for _, state := range []string{"results-selected", "validation-error", "live-applied", "timeline-detail", "storage-health", "screenshot", "loading", "empty", "error", "long-content"} {
		if !coveredStates[state] {
			t.Errorf("state %q is uncovered", state)
		}
	}
	specs, err := catalogue.Matrix(catalogue.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(theme.IDs()) != fixture.ThemeCount || len(specs) != fixture.EntryCount {
		t.Fatalf("themes=%d entries=%d, want %d/%d", len(theme.IDs()), len(specs), fixture.ThemeCount, fixture.EntryCount)
	}
	wantFirst := "dense-palette/catppuccin/wide/picker-search-selected.png"
	wantLast := "calm-cards/vesper/phone-keyboard/long-content.png"
	if specs[0].RelativePath() != wantFirst || specs[len(specs)-1].RelativePath() != wantLast {
		t.Fatalf("matrix endpoints = %q .. %q", specs[0].RelativePath(), specs[len(specs)-1].RelativePath())
	}
}

func TestHYPCAT002EveryViewportRendersWithoutPanicOrLostFrameEdges(t *testing.T) {
	for _, design := range catalogue.Designs() {
		for _, viewport := range catalogue.Viewports() {
			for _, scenario := range catalogue.Scenarios() {
				spec := catalogue.Spec{Design: design, ThemeID: "catppuccin", Viewport: viewport, Scenario: scenario}
				frame, err := catalogue.Render(spec)
				if err != nil {
					t.Fatalf("%s: %v", spec.RelativePath(), err)
				}
				if frame.Width() != viewport.Width || frame.Height() != viewport.Height {
					t.Fatalf("%s dimensions = %dx%d", spec.RelativePath(), frame.Width(), frame.Height())
				}
				for _, point := range [][2]int{{0, 0}, {frame.Width() - 1, 0}, {0, frame.Height() - 1}, {frame.Width() - 1, frame.Height() - 1}} {
					cell, ok := frame.CellAt(point[0], point[1])
					if !ok || cell.Style.Background == "" {
						t.Fatalf("%s lost styled edge at %v: %+v", spec.RelativePath(), point, cell)
					}
				}
			}
		}
	}
}

func TestHYPCAT002EverySupportedThemeRendersItsOwnPalette(t *testing.T) {
	design := catalogue.Designs()[0]
	viewport := catalogue.Viewports()[1]
	scenario := catalogue.Scenarios()[0]
	for _, themeID := range theme.IDs() {
		t.Run(themeID, func(t *testing.T) {
			frame, err := catalogue.Render(catalogue.Spec{Design: design, ThemeID: themeID, Viewport: viewport, Scenario: scenario})
			if err != nil {
				t.Fatal(err)
			}
			palette, err := theme.Builtin(themeID)
			if err != nil {
				t.Fatal(err)
			}
			cell, ok := frame.CellAt(frame.Width()-1, frame.Height()-1)
			if !ok || cell.Style.Background != palette.Background {
				t.Fatalf("bottom-right background = %q, want %q", cell.Style.Background, palette.Background)
			}
			if encoded, err := view.PNG(frame); err != nil || len(encoded) == 0 {
				t.Fatalf("PNG bytes = %d, error = %v", len(encoded), err)
			}
		})
	}
}

func TestHYPCAT002RepresentativeFramesPreserveContentAndVisualDesign(t *testing.T) {
	fixture := loadFixture(t)
	specs, err := catalogue.Matrix(catalogue.Selection{
		ThemeIDs:    []string{"catppuccin"},
		ViewportIDs: []string{"desktop", "phone-keyboard"},
		ScenarioIDs: []string{"picker-search-selected", "config-validation", "long-content"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantText := map[string]string{
		"picker-search-selected": "Source: Codex Recall",
		"config-validation":      "Refresh interval must be positive",
		"long-content":           "Long content",
	}
	seenHashes := make(map[string]map[string]bool)
	for _, spec := range specs {
		frame, err := catalogue.Render(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec.RelativePath(), err)
		}
		plain := frameText(frame)
		if !strings.Contains(strings.ToLower(plain), strings.ToLower(wantText[spec.Scenario.ID])) {
			t.Fatalf("%s lost meaningful state text %q\n%s", spec.RelativePath(), wantText[spec.Scenario.ID], plain)
		}
		encoded, err := view.PNG(frame)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("%s: %v", spec.RelativePath(), err)
		}
		if got := decoded.Bounds().Size(); got.X != spec.Viewport.Width*view.PNGCellWidth || got.Y != spec.Viewport.Height*view.PNGCellHeight {
			t.Fatalf("%s pixels = %v", spec.RelativePath(), got)
		}
		sum := sha256.Sum256(encoded)
		gotHash := hex.EncodeToString(sum[:])
		if wantHash := fixture.RenderHashes[spec.RelativePath()]; gotHash != wantHash {
			t.Fatalf("%s visual hash = %s, want %s", spec.RelativePath(), gotHash, wantHash)
		}
		group := spec.Viewport.ID + "/" + spec.Scenario.ID
		if seenHashes[group] == nil {
			seenHashes[group] = make(map[string]bool)
		}
		seenHashes[group][gotHash] = true
	}
	for group, hashes := range seenHashes {
		if len(hashes) != len(catalogue.Designs()) {
			t.Errorf("%s has %d distinct rendered designs, want %d", group, len(hashes), len(catalogue.Designs()))
		}
	}
}

func TestHYPCAT002MatrixFrameFingerprints(t *testing.T) {
	fixture := loadFixture(t)
	specs, err := catalogue.Matrix(catalogue.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	appendInteger := func(data []byte, value int) []byte {
		return binary.BigEndian.AppendUint64(data, uint64(value))
	}
	appendString := func(data []byte, value string) []byte {
		data = appendInteger(data, len(value))
		return append(data, value...)
	}
	appendBool := func(data []byte, value bool) []byte {
		if value {
			return append(data, 1)
		}
		return append(data, 0)
	}
	digests := make(map[string]hash.Hash)
	keys := make([]string, 0, fixture.ContactSheetCount)
	for _, spec := range specs {
		key := strings.Join([]string{spec.Design.ID, spec.ThemeID, spec.Viewport.ID}, "/")
		digest := digests[key]
		if digest == nil {
			digest = sha256.New()
			digest.Write([]byte("herdr-catalogue-frame-contact-sheet-v1\x00"))
			digests[key] = digest
			keys = append(keys, key)
		}
		frame, err := catalogue.Render(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec.RelativePath(), err)
		}
		encoded := make([]byte, 0, frame.Width()*frame.Height()*32)
		encoded = appendString(encoded, spec.RelativePath())
		encoded = appendInteger(encoded, frame.Width())
		encoded = appendInteger(encoded, frame.Height())
		for y := 0; y < frame.Height(); y++ {
			for x := 0; x < frame.Width(); x++ {
				cell, ok := frame.CellAt(x, y)
				if !ok {
					t.Fatalf("%s missing cell at %d,%d", spec.RelativePath(), x, y)
				}
				encoded = appendString(encoded, cell.Text)
				encoded = appendString(encoded, string(cell.Style.Foreground))
				encoded = appendString(encoded, string(cell.Style.Background))
				encoded = appendInteger(encoded, cell.Width)
				encoded = appendBool(encoded, cell.Continuation)
				encoded = appendBool(encoded, cell.Style.Bold)
				encoded = appendBool(encoded, cell.Style.Dim)
				encoded = appendBool(encoded, cell.Style.Underline)
			}
		}
		digest.Write(encoded)
	}
	for _, key := range keys {
		if _, ok := fixture.MatrixFrameHashes[key]; !ok {
			t.Errorf("matrix frame hashes missing key %q (got %s)", key, hex.EncodeToString(digests[key].Sum(nil)))
		}
	}
	extra := make([]string, 0)
	for key := range fixture.MatrixFrameHashes {
		if _, ok := digests[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		t.Errorf("matrix frame hashes contain extra key %q", key)
	}
	if t.Failed() {
		return
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			gotHash := hex.EncodeToString(digests[key].Sum(nil))
			if gotHash != fixture.MatrixFrameHashes[key] {
				t.Fatalf("frame hash = %s, want %s", gotHash, fixture.MatrixFrameHashes[key])
			}
		})
	}
}

func TestHYPCAT002RepresentativeContactSheetsPreserveReviewArtifact(t *testing.T) {
	fixture := loadFixture(t)
	cases := []struct {
		designID   string
		themeID    string
		viewportID string
	}{
		{designID: "dense-palette", themeID: "catppuccin", viewportID: "wide"},
		{designID: "dense-palette", themeID: "gruvbox", viewportID: "desktop"},
		{designID: "dense-palette", themeID: "tokyo-night", viewportID: "phone"},
		{designID: "dense-palette", themeID: "vesper", viewportID: "phone-keyboard"},
		{designID: "split-inspector", themeID: "rose-pine", viewportID: "wide"},
		{designID: "split-inspector", themeID: "nord", viewportID: "desktop"},
		{designID: "split-inspector", themeID: "solarized", viewportID: "phone"},
		{designID: "split-inspector", themeID: "one-dark", viewportID: "phone-keyboard"},
		{designID: "calm-cards", themeID: "catppuccin-latte", viewportID: "wide"},
		{designID: "calm-cards", themeID: "kanagawa", viewportID: "desktop"},
		{designID: "calm-cards", themeID: "dracula", viewportID: "phone"},
		{designID: "calm-cards", themeID: "terminal", viewportID: "phone-keyboard"},
	}
	pairs := make(map[string]bool, len(cases))
	for _, tc := range cases {
		pair := tc.designID + "/" + tc.viewportID
		if pairs[pair] {
			t.Fatalf("duplicate contact-sheet pair %q", pair)
		}
		pairs[pair] = true
	}
	for _, design := range catalogue.Designs() {
		for _, viewport := range catalogue.Viewports() {
			pair := design.ID + "/" + viewport.ID
			if !pairs[pair] {
				t.Fatalf("missing contact-sheet pair %q", pair)
			}
		}
	}
	for _, tc := range cases {
		path := strings.Join([]string{"contact-sheets", tc.designID, tc.themeID, tc.viewportID + ".png"}, "/")
		t.Run(strings.TrimSuffix(strings.TrimPrefix(path, "contact-sheets/"), ".png"), func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "catalogue")
			manifest, err := catalogue.Export(context.Background(), output, catalogue.Selection{
				DesignIDs:   []string{tc.designID},
				ThemeIDs:    []string{tc.themeID},
				ViewportIDs: []string{tc.viewportID},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.ContactSheets) != 1 || manifest.ContactSheets[0].Path != path {
				t.Fatalf("contact sheets = %+v, want only %q", manifest.ContactSheets, path)
			}
			data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			gotHash := hex.EncodeToString(sum[:])
			if manifest.ContactSheets[0].SHA256 != gotHash {
				t.Fatalf("%s manifest hash = %s, artifact hash = %s", path, manifest.ContactSheets[0].SHA256, gotHash)
			}
			wantHash, ok := fixture.ContactSheetHashes[path]
			if !ok {
				t.Fatalf("%s has no golden hash", path)
			}
			if gotHash != wantHash {
				t.Fatalf("%s visual hash = %s, want %s", path, gotHash, wantHash)
			}
		})
	}
}

func TestHYPCAT003ExportIsDeterministicInspectableAndRefusesUnsafeTargets(t *testing.T) {
	selection := catalogue.Selection{DesignIDs: []string{"split-inspector"}, ThemeIDs: []string{"nord"}, ScenarioIDs: []string{"picker-search-selected", "diagnostic-screenshot"}}
	export := func(root string) catalogue.Manifest {
		t.Helper()
		manifest, err := catalogue.Export(context.Background(), root, selection)
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	firstDir, secondDir := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	first, second := export(firstDir), export(secondDir)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("manifests differ\n%s\n%s", firstJSON, secondJSON)
	}
	if len(first.Entries) != 8 || len(first.ContactSheets) != 4 {
		t.Fatalf("entries/sheets = %d/%d", len(first.Entries), len(first.ContactSheets))
	}
	wantSpecs, err := catalogue.Matrix(selection)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range first.Entries {
		if entry.Path != wantSpecs[i].RelativePath() {
			t.Fatalf("entry %d path = %q, want matrix path %q", i, entry.Path, wantSpecs[i].RelativePath())
		}
		wantPixelWidth := entry.CellWidth * view.PNGCellWidth
		wantPixelHeight := entry.CellHeight * view.PNGCellHeight
		if entry.PixelWidth != wantPixelWidth || entry.PixelHeight != wantPixelHeight {
			t.Fatalf("%s manifest pixels = %dx%d, want %dx%d", entry.Path, entry.PixelWidth, entry.PixelHeight, wantPixelWidth, wantPixelHeight)
		}
		data, err := os.ReadFile(filepath.Join(firstDir, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("decode %s: %v", entry.Path, err)
		}
	}
	for _, name := range []string{"index.json", "index.html"} {
		if _, err := os.Stat(filepath.Join(firstDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	htmlIndex, err := os.ReadFile(filepath.Join(firstDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sheet := range first.ContactSheets {
		data, err := os.ReadFile(filepath.Join(firstDir, filepath.FromSlash(sheet.Path)))
		if err != nil {
			t.Errorf("missing contact sheet %s: %v", sheet.Path, err)
			continue
		}
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("decode contact sheet %s: %v", sheet.Path, err)
		}
		if !bytes.Contains(htmlIndex, []byte(`href="`+sheet.Path+`"`)) {
			t.Errorf("HTML index does not link contact sheet %s", sheet.Path)
		}
	}
	if err := filepath.WalkDir(firstDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(firstDir, path)
		if err != nil {
			return err
		}
		firstBytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		secondBytes, err := os.ReadFile(filepath.Join(secondDir, relative))
		if err != nil {
			return err
		}
		if !bytes.Equal(firstBytes, secondBytes) {
			t.Errorf("exported bytes differ for %s", relative)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Export(context.Background(), nonEmpty, selection); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty export error = %v", err)
	}
	empty := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogue.Export(context.Background(), empty, selection); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing empty export error = %v", err)
	}
	cancelledOutput := filepath.Join(t.TempDir(), "cancelled")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalogue.Export(cancelled, cancelledOutput, selection); err == nil {
		t.Fatal("cancelled export succeeded")
	}
	if _, err := os.Lstat(cancelledOutput); !os.IsNotExist(err) {
		t.Fatalf("cancelled export left published output: %v", err)
	}
	lateCancelledOutput := filepath.Join(t.TempDir(), "late-cancelled")
	lateCancellation := &stepContext{cancelAfter: len(wantSpecs) + 1}
	if _, err := catalogue.Export(lateCancellation, lateCancelledOutput, selection); err == nil {
		t.Fatal("export ignored cancellation during contact-sheet generation")
	}
	if _, err := os.Lstat(lateCancelledOutput); !os.IsNotExist(err) {
		t.Fatalf("late-cancelled export left published output: %v", err)
	}
	commitCancelledOutput := filepath.Join(t.TempDir(), "commit-cancelled")
	commitCancellation := &stepContext{cancelAfter: len(wantSpecs) + len(first.ContactSheets) + 2}
	if _, err := catalogue.Export(commitCancellation, commitCancelledOutput, selection); err == nil {
		t.Fatal("export ignored cancellation at publication commit point")
	}
	if _, err := os.Lstat(commitCancelledOutput); !os.IsNotExist(err) {
		t.Fatalf("commit-cancelled export left published output: %v", err)
	}
	racedOutput := filepath.Join(t.TempDir(), "raced")
	raceContext := &stepContext{
		actionAt: len(wantSpecs) + len(first.ContactSheets) + 1,
		action:   func() error { return os.Mkdir(racedOutput, 0o755) },
	}
	if _, err := catalogue.Export(raceContext, racedOutput, selection); err == nil || !strings.Contains(err.Error(), "publish catalogue") {
		t.Fatalf("raced publication error = %v", err)
	}
	entries, err := os.ReadDir(racedOutput)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-replace publication changed raced target: entries=%v error=%v", entries, err)
	}
	if _, err := catalogue.Matrix(catalogue.Selection{ThemeIDs: []string{"../escape"}}); err == nil {
		t.Fatal("unsafe/unknown filter accepted")
	}
}

type stepContext struct {
	calls       int
	cancelAfter int
	actionAt    int
	action      func() error
	actionErr   error
}

func (c *stepContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stepContext) Done() <-chan struct{}       { return nil }
func (c *stepContext) Value(any) any               { return nil }
func (c *stepContext) Err() error {
	c.calls++
	if c.calls == c.actionAt && c.action != nil {
		c.actionErr = c.action()
	}
	if c.actionErr != nil {
		return c.actionErr
	}
	if c.cancelAfter > 0 && c.calls >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}

func frameText(frame *view.Frame) string {
	var output strings.Builder
	for y := 0; y < frame.Height(); y++ {
		for x := 0; x < frame.Width(); x++ {
			cell, _ := frame.CellAt(x, y)
			if cell.Continuation {
				continue
			}
			if cell.Text == "" {
				output.WriteByte(' ')
			} else {
				output.WriteString(cell.Text)
			}
		}
		output.WriteByte('\n')
	}
	return output.String()
}
