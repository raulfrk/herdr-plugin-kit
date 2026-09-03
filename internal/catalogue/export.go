package catalogue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type Manifest struct {
	Version       int                    `json:"version"`
	Generator     string                 `json:"generator"`
	Entries       []ManifestEntry        `json:"entries"`
	ContactSheets []ContactSheetManifest `json:"contact_sheets"`
}

type ManifestEntry struct {
	Path        string   `json:"path"`
	SHA256      string   `json:"sha256"`
	ThemeID     string   `json:"theme"`
	ViewportID  string   `json:"viewport"`
	ScenarioID  string   `json:"scenario"`
	State       string   `json:"state"`
	Surfaces    []string `json:"surfaces"`
	CellWidth   int      `json:"cell_width"`
	CellHeight  int      `json:"cell_height"`
	PixelWidth  int      `json:"pixel_width"`
	PixelHeight int      `json:"pixel_height"`
}

type ContactSheetManifest struct {
	Path        string   `json:"path"`
	SHA256      string   `json:"sha256"`
	ThemeID     string   `json:"theme"`
	ViewportID  string   `json:"viewport"`
	Scenarios   []string `json:"scenarios"`
	PixelWidth  int      `json:"pixel_width"`
	PixelHeight int      `json:"pixel_height"`
}

func Export(ctx context.Context, outputDir string, selection Selection) (Manifest, error) {
	if outputDir == "" {
		return Manifest{}, fmt.Errorf("output directory is required")
	}
	specs, err := Matrix(selection)
	if err != nil {
		return Manifest{}, err
	}
	if len(specs) == 0 {
		return Manifest{}, fmt.Errorf("selection produced no catalogue entries")
	}
	stagingDir, err := prepareOutput(outputDir)
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stagingDir)
	manifest := Manifest{Version: manifestVersion, Generator: "herdr-plugin-kit catalogue", Entries: make([]ManifestEntry, 0, len(specs))}
	images := make(map[string][][]byte)
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		frame, err := Render(spec)
		if err != nil {
			return Manifest{}, err
		}
		encoded, err := view.PNG(frame)
		if err != nil {
			return Manifest{}, fmt.Errorf("render %s: %w", spec.RelativePath(), err)
		}
		if err := writeRelative(stagingDir, spec.RelativePath(), encoded); err != nil {
			return Manifest{}, err
		}
		manifest.Entries = append(manifest.Entries, ManifestEntry{Path: spec.RelativePath(), SHA256: digest(encoded), ThemeID: spec.ThemeID, ViewportID: spec.Viewport.ID, ScenarioID: spec.Scenario.ID, State: spec.Scenario.State, Surfaces: append([]string(nil), spec.Scenario.Surfaces...), CellWidth: spec.Viewport.Width, CellHeight: spec.Viewport.Height, PixelWidth: spec.Viewport.Width * view.PNGCellWidth, PixelHeight: spec.Viewport.Height * view.PNGCellHeight})
		key := strings.Join([]string{spec.ThemeID, spec.Viewport.ID}, "/")
		images[key] = append(images[key], encoded)
	}
	keys := make([]string, 0, len(images))
	for key := range images {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		parts := strings.Split(key, "/")
		encoded, width, height, err := contactSheet(images[key])
		if err != nil {
			return Manifest{}, fmt.Errorf("contact sheet %s: %w", key, err)
		}
		path := filepath.ToSlash(filepath.Join("contact-sheets", parts[0], parts[1]+".png"))
		if err := writeRelative(stagingDir, path, encoded); err != nil {
			return Manifest{}, err
		}
		scenarioIDs := selectedScenarioIDs(specs, parts[0], parts[1])
		manifest.ContactSheets = append(manifest.ContactSheets, ContactSheetManifest{Path: path, SHA256: digest(encoded), ThemeID: parts[0], ViewportID: parts[1], Scenarios: scenarioIDs, PixelWidth: width, PixelHeight: height})
	}
	index, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	index = append(index, '\n')
	if err := writeRelative(stagingDir, "index.json", index); err != nil {
		return Manifest{}, err
	}
	var html bytes.Buffer
	if err := indexTemplate.Execute(&html, manifest); err != nil {
		return Manifest{}, err
	}
	if err := writeRelative(stagingDir, "index.html", html.Bytes()); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := os.Chmod(stagingDir, 0o755); err != nil {
		return Manifest{}, fmt.Errorf("prepare catalogue publication: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := publishNoReplace(stagingDir, outputDir); err != nil {
		return Manifest{}, fmt.Errorf("publish catalogue: %w", err)
	}
	return manifest, nil
}

func prepareOutput(path string) (string, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("output %q already exists", path)
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return "", fmt.Errorf("read output: %w", readErr)
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("output directory %q is not empty", path)
		}
		return "", fmt.Errorf("output directory %q already exists; choose a path that does not exist", path)
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect output: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve output: %w", err)
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create output parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".herdr-catalogue-stage-")
	if err != nil {
		return "", fmt.Errorf("create private staging directory: %w", err)
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("secure staging directory: %w", err)
	}
	return staging, nil
}

func writeRelative(root, relative string, data []byte) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return fmt.Errorf("output path escapes root: %q", relative)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relative, err)
	}
	return nil
}

func contactSheet(encodedImages [][]byte) ([]byte, int, int, error) {
	if len(encodedImages) == 0 {
		return nil, 0, 0, fmt.Errorf("no images")
	}
	decoded := make([]image.Image, len(encodedImages))
	for i, encoded := range encodedImages {
		img, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			return nil, 0, 0, err
		}
		decoded[i] = img
	}
	thumbWidth := max(1, decoded[0].Bounds().Dx()/4)
	thumbHeight := max(1, decoded[0].Bounds().Dy()/4)
	columns := min(2, len(decoded))
	rows := (len(decoded) + columns - 1) / columns
	gap := 8
	width := columns*thumbWidth + (columns-1)*gap
	height := rows*thumbHeight + (rows-1)*gap
	sheet := image.NewRGBA(image.Rect(0, 0, width, height))
	for i, source := range decoded {
		offsetX := (i % columns) * (thumbWidth + gap)
		offsetY := (i / columns) * (thumbHeight + gap)
		bounds := source.Bounds()
		for y := range thumbHeight {
			for x := range thumbWidth {
				sheet.Set(offsetX+x, offsetY+y, source.At(bounds.Min.X+x*4, bounds.Min.Y+y*4))
			}
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, sheet); err != nil {
		return nil, 0, 0, err
	}
	return output.Bytes(), width, height, nil
}

func selectedScenarioIDs(specs []Spec, themeID, viewport string) []string {
	var result []string
	for _, spec := range specs {
		if spec.ThemeID == themeID && spec.Viewport.ID == viewport {
			result = append(result, spec.Scenario.ID)
		}
	}
	return result
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

var indexTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Herdr UI catalogue</title><style>body{font:14px sans-serif;margin:2rem;background:#111;color:#eee}img{max-width:100%;border:1px solid #555}section{margin:2rem 0}code{color:#9dd}</style></head><body>
<h1>Herdr UI catalogue</h1><p>{{len .Entries}} scenarios · {{len .ContactSheets}} contact sheets · deterministic manifest v{{.Version}}</p>
{{range .ContactSheets}}<section><h2>{{.ThemeID}} / {{.ViewportID}}</h2><p><code>{{range $i, $v := .Scenarios}}{{if $i}}, {{end}}{{$v}}{{end}}</code></p><a href="{{.Path}}"><img src="{{.Path}}" alt="{{.ThemeID}} {{.ViewportID}} contact sheet"></a></section>{{end}}
</body></html>
`))
