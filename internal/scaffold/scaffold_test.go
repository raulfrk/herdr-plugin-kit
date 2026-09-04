package scaffold

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestGenerateCreatesOnlyTheValidatedPluginContract(t *testing.T) {
	output := filepath.Join(t.TempDir(), "plugin")
	options := Options{ID: "example.search", Name: "Example Search", Description: "Search safely.", Output: output}
	if err := Generate(options); err != nil {
		t.Fatal(err)
	}
	if err := Validate(output); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	var files []string
	if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(output, path)
		if err == nil {
			files = append(files, filepath.ToSlash(relative))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	want := []string{
		"LICENSE", "README.md", "cmd/example.search/main.go", "cmd/example.search/main_test.go", "config.example.toml",
		"go.mod", "herdr-plugin.toml", "plugin-kit.json",
	}
	if !slices.Equal(files, want) {
		t.Fatalf("generated files = %v, want %v", files, want)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(data), []byte("catalogue")) {
			t.Fatalf("%s contains catalogue content", file)
		}
	}
	goMod, _ := os.ReadFile(filepath.Join(output, "go.mod"))
	if bytes.Contains(goMod, []byte("replace")) || !bytes.Contains(goMod, []byte(KitModule+" "+KitVersion)) {
		t.Fatalf("generated go.mod = %s", goMod)
	}
	license, _ := os.ReadFile(filepath.Join(output, "LICENSE"))
	wantLicense, _ := templates.ReadFile("templates/LICENSE")
	if !bytes.Equal(license, wantLicense) {
		t.Fatal("generated license differs from embedded Apache-2.0 text")
	}
}

func TestGeneratedProjectBuildsWithoutChangingItsGoMod(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs(filepath.Join(workingDirectory, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := filepath.Join(root, "plugin")
	if err := Generate(Options{ID: "example.build", Name: "Build Example", Output: output}); err != nil {
		t.Fatal(err)
	}
	goModPath := filepath.Join(output, "go.mod")
	before, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "go.work")
	workspaceData := []byte("go 1.27\n\nuse " + output + "\n\nreplace " + KitModule + " " + KitVersion + " => " + repository + "\n")
	if err := os.WriteFile(workspace, workspaceData, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "./...")
	command.Dir = output
	command.Env = append(os.Environ(), "GOWORK="+workspace, "GOTOOLCHAIN=go1.27.0")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated go test error = %v\n%s", err, data)
	}
	binary := filepath.Join(root, "plugin-bin")
	command = exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/example.build")
	command.Dir = output
	command.Env = append(os.Environ(), "GOWORK="+workspace, "GOTOOLCHAIN=go1.27.0")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated go build error = %v\n%s", err, data)
	}
	configDirectory := filepath.Join(root, "config")
	stateDirectory := filepath.Join(root, "state")
	command = exec.Command(binary, "action", "health")
	command.Env = append(os.Environ(), "HERDR_PLUGIN_CONFIG_DIR="+configDirectory, "HERDR_PLUGIN_STATE_DIR="+stateDirectory)
	healthData, err := command.Output()
	if err != nil {
		t.Fatalf("generated health action error = %v", err)
	}
	var health struct {
		Version          int             `json:"version"`
		Interface        string          `json:"interface"`
		InterfaceVersion int             `json:"interface_version"`
		Method           string          `json:"method"`
		Payload          json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(healthData, &health); err != nil || health.Version != 1 ||
		health.Interface != "health" || health.InterfaceVersion != 1 || health.Method != "check" ||
		!bytes.Equal(health.Payload, []byte(`{"ok":true}`)) {
		t.Fatalf("generated health response = %s, error = %v", healthData, err)
	}
	after, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte("replace")) {
		t.Fatalf("generated build changed go.mod:\n%s", after)
	}
}

func TestGenerateNeverOverwritesAndConcurrentPublicationHasOneWinner(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "plugin")
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(output, "owned")
		if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Generate(Options{ID: "example.safe", Name: "Safe", Output: output}); err == nil {
			t.Fatal("Generate() overwrote an existing directory")
		}
		if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
			t.Fatalf("existing marker = %q, %v", data, err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		parent := t.TempDir()
		output := filepath.Join(parent, "plugin")
		options := Options{ID: "example.race", Name: "Race", Output: output}
		const contenders = 8
		results := make(chan error, contenders)
		var group sync.WaitGroup
		for range contenders {
			group.Add(1)
			go func() {
				defer group.Done()
				results <- Generate(options)
			}()
		}
		group.Wait()
		close(results)
		succeeded := 0
		for err := range results {
			if err == nil {
				succeeded++
			} else if !strings.Contains(err.Error(), "already exists") {
				t.Errorf("losing Generate() error = %v", err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("successful publishers = %d, want 1", succeeded)
		}
		if err := Validate(output); err != nil {
			t.Fatalf("published project invalid: %v", err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "plugin" {
			t.Fatalf("publication left sibling artifacts: %v", entries)
		}
	})

	t.Run("parent substitution", func(t *testing.T) {
		outer := t.TempDir()
		parent := filepath.Join(outer, "parent")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		original := filepath.Join(outer, "original")
		replacement := filepath.Join(outer, "replacement")
		err := generate(Options{ID: "example.parent", Name: "Parent", Output: filepath.Join(parent, "plugin")}, generationHooks{
			beforePublish: func() {
				if err := os.Rename(parent, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(replacement, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(replacement, parent); err != nil {
					t.Fatal(err)
				}
			},
		})
		if err == nil || !strings.Contains(err.Error(), "parent changed") {
			t.Fatalf("parent substitution error = %v", err)
		}
		for _, candidate := range []string{filepath.Join(original, "plugin"), filepath.Join(replacement, "plugin")} {
			if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("parent substitution published %s: %v", candidate, err)
			}
		}
		entries, err := os.ReadDir(original)
		if err != nil || len(entries) != 0 {
			t.Fatalf("anchored cleanup entries = %v, error = %v", entries, err)
		}
	})

	t.Run("sync failure rolls back", func(t *testing.T) {
		parent := t.TempDir()
		output := filepath.Join(parent, "plugin")
		err := generate(Options{ID: "example.sync", Name: "Sync", Output: output}, generationHooks{
			syncParent: func(*os.File) error { return errors.New("injected sync failure") },
		})
		if err == nil || !strings.Contains(err.Error(), "sync published output") {
			t.Fatalf("sync failure error = %v", err)
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sync failure left published output: %v", err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sync rollback entries = %v, error = %v", entries, err)
		}
	})

	t.Run("sync failure preserves substituted destination", func(t *testing.T) {
		parent := t.TempDir()
		output := filepath.Join(parent, "plugin")
		publishedCopy := filepath.Join(parent, "published-copy")
		marker := filepath.Join(output, "owned")
		err := generate(Options{ID: "example.substitute", Name: "Substitute", Output: output}, generationHooks{
			syncParent: func(*os.File) error {
				if err := os.Rename(output, publishedCopy); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(output, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				return errors.New("injected sync failure")
			},
		})
		if err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("substituted sync failure error = %v", err)
		}
		if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
			t.Fatalf("replacement marker = %q, %v", data, err)
		}
		if err := Validate(publishedCopy); err != nil {
			t.Fatalf("original published tree changed: %v", err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".rollback-") {
				t.Fatalf("rollback quarantine remained: %s", entry.Name())
			}
		}
	})
}

func TestGenerateRejectsInvalidOptionsWithoutPublishing(t *testing.T) {
	parent := t.TempDir()
	for name, options := range map[string]Options{
		"id":     {ID: "Invalid ID", Name: "Valid", Output: filepath.Join(parent, "bad-id")},
		"name":   {ID: "valid.id", Name: "", Output: filepath.Join(parent, "bad-name")},
		"output": {ID: "valid.id", Name: "Valid"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Generate(options); err == nil {
				t.Fatal("invalid generation succeeded")
			}
			if options.Output != "" {
				if _, err := os.Lstat(options.Output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid generation published output: %v", err)
				}
			}
		})
	}
}

func TestValidateRejectsEveryCrossContractDrift(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"kit identity": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["name"] = "Changed"
			writeJSONFile(t, path, value)
		},
		"Herdr identity": func(t *testing.T, root string) {
			mutateHerdr(t, root, func(value *herdrManifest) { value.ID = "example.changed" })
		},
		"minimum Herdr": func(t *testing.T, root string) {
			mutateHerdr(t, root, func(value *herdrManifest) { value.MinHerdrVersion = "9.0.0" })
		},
		"shell build": func(t *testing.T, root string) {
			mutateHerdr(t, root, func(value *herdrManifest) { value.Build[0].Command = []string{"sh", "-c", "go build"} })
		},
		"pane placement": func(t *testing.T, root string) {
			mutateHerdr(t, root, func(value *herdrManifest) { value.Panes[0].Placement = "split" })
		},
		"action identity": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["actions"].([]any)[0].(map[string]any)["id"] = "changed"
			writeJSONFile(t, path, value)
		},
		"manifest args": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["args"] = []string{"extra"}
			writeJSONFile(t, path, value)
		},
		"matching action title drift": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["actions"].([]any)[0].(map[string]any)["title"] = "Changed"
			writeJSONFile(t, path, value)
			mutateHerdr(t, root, func(value *herdrManifest) { value.Actions[0].Title = "Changed" })
		},
		"matching action description drift": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["actions"].([]any)[0].(map[string]any)["description"] = "Changed"
			writeJSONFile(t, path, value)
			mutateHerdr(t, root, func(value *herdrManifest) { value.Actions[0].Description = "Changed" })
		},
		"pane title": func(t *testing.T, root string) {
			mutateHerdr(t, root, func(value *herdrManifest) { value.Panes[0].Title = "Changed" })
		},
		"replace directive": func(t *testing.T, root string) {
			path := filepath.Join(root, "go.mod")
			data, _ := os.ReadFile(path)
			data = append(data, []byte("\nreplace\tgithub.com/example/old => ./local\n")...)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"commented require": func(t *testing.T, root string) {
			data := "module example.com/herdr/example.valid\n\ngo 1.27\n\n// require " + KitModule + " " + KitVersion + "\n"
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"license": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("not Apache"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"missing runtime environment": func(t *testing.T, root string) {
			path := filepath.Join(root, "cmd", "example.valid", "main.go")
			mutateTextAll(t, path, "HERDR_PLUGIN_STATE_DIR", "STATE_DIRECTORY")
		},
		"source plugin identity": func(t *testing.T, root string) {
			path := filepath.Join(root, "cmd", "example.valid", "main.go")
			mutateTextAll(t, path, `pluginID   = "example.valid"`, `pluginID   = "example.changed"`)
		},
		"source plugin name": func(t *testing.T, root string) {
			path := filepath.Join(root, "cmd", "example.valid", "main.go")
			mutateTextAll(t, path, `pluginName = "Valid"`, `pluginName = "Changed"`)
		},
		"source health payload": func(t *testing.T, root string) {
			path := filepath.Join(root, "cmd", "example.valid", "main.go")
			mutateTextAll(t, path, `{"ok":true}`, `{"ok":false}`)
		},
		"configuration example": func(t *testing.T, root string) {
			path := filepath.Join(root, "config.example.toml")
			mutateTextAll(t, path, "catppuccin", "unknown")
		},
		"invalid Go": func(t *testing.T, root string) {
			path := filepath.Join(root, "cmd", "example.valid", "main.go")
			if err := os.WriteFile(path, []byte("package main\nfunc {"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			path := filepath.Join(root, "README.md")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
				t.Fatal(err)
			}
		},
		"oversized file": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "README.md"), bytes.Repeat([]byte("x"), maxFileBytes+1), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"catalogue leak": func(t *testing.T, root string) {
			directory := filepath.Join(root, "catalogue")
			if err := os.Mkdir(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "index.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "plugin")
			if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
				t.Fatal(err)
			}
			mutate(t, root)
			if err := Validate(root); err == nil {
				t.Fatal("contract drift passed validation")
			}
		})
	}
}

func TestValidateAllowsFuturePluginSourceFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.extend", Name: "Extend", Output: root}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "internal", "feature")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "feature.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root); err != nil {
		t.Fatalf("future source extension rejected: %v", err)
	}
}

func TestHerdrEncodingUsesArgvArraysAndOverlayPanes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.quote", Name: `Quoted "Name"`, Description: "line one\nline two", Output: root}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded herdrManifest
	if err := toml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != `Quoted "Name"` || decoded.Description != "line one\nline two" {
		t.Fatalf("escaped metadata changed: %#v", decoded)
	}
	for _, pane := range decoded.Panes {
		if pane.Placement != "overlay" || len(pane.Command) != 2 {
			t.Fatalf("pane contract = %#v", pane)
		}
	}
}

func decodeJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateHerdr(t *testing.T, root string, mutate func(*herdrManifest)) {
	t.Helper()
	path := filepath.Join(root, "herdr-plugin.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value herdrManifest
	if err := toml.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	mutate(&value)
	data, err = toml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateText(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), old, replacement, 1)
	if changed == string(data) {
		t.Fatalf("mutation target %q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateTextAll(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(data), old, replacement)
	if changed == string(data) {
		t.Fatalf("mutation target %q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
}
