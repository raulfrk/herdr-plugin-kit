package scaffold

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/raulfrk/herdr-plugin-kit/internal/mutation"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
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
		".gitignore", ".gremlins.yaml", "LICENSE", "Makefile", "README.md",
		"cmd/example.search/main.go", "cmd/example.search/main_test.go", "config.example.toml",
		"docs/mutation-baseline.md", "docs/test-strategy.md", "go.mod", "herdr-plugin.toml",
		"mutation-policy.json", "plugin-kit.json", "scripts/mutation-run",
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
	readme, _ := os.ReadFile(filepath.Join(output, "README.md"))
	wantKitLabel := "Generated with Herdr Plugin Kit " + KitVersion + "."
	if !bytes.Contains(readme, []byte(wantKitLabel)) {
		t.Fatalf("generated README does not document %q", wantKitLabel)
	}
	wantBuild := "go build -mod=mod -buildvcs=false -o ./plugin ./cmd/" + options.ID
	if !bytes.Contains(readme, []byte(wantBuild)) {
		t.Fatalf("generated README does not document %q", wantBuild)
	}
	var herdr herdrManifest
	herdrData, _ := os.ReadFile(filepath.Join(output, "herdr-plugin.toml"))
	if err := toml.Unmarshal(herdrData, &herdr); err != nil {
		t.Fatal(err)
	}
	wantBuildArgs := []string{"go", "build", "-mod=mod", "-buildvcs=false", "-o", "./plugin", "./cmd/" + options.ID}
	if !slices.Equal(herdr.Build[0].Command, wantBuildArgs) {
		t.Fatalf("generated build command = %q, want %q", herdr.Build[0].Command, wantBuildArgs)
	}
	license, _ := os.ReadFile(filepath.Join(output, "LICENSE"))
	wantLicense, _ := templates.ReadFile("templates/LICENSE")
	if !bytes.Equal(license, wantLicense) {
		t.Fatal("generated license differs from embedded Apache-2.0 text")
	}
}

func TestGeneratedMutationWrapperDownloadsPinnedKitBeforeResolvingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.wrapper", Name: "Wrapper", Output: root}); err != nil {
		t.Fatal(err)
	}

	testRoot := t.TempDir()
	binDir := filepath.Join(testRoot, "bin")
	kitDir := filepath.Join(testRoot, "kit")
	marker := filepath.Join(testRoot, "downloaded")
	result := filepath.Join(testRoot, "result")
	if err := os.MkdirAll(filepath.Join(kitDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := `#!/bin/sh
case " $* " in
  *" {{if .Replace}}HERDR_REPLACED{{else}}{{.Path}}@{{.Version}}{{end}} "*)
    echo "github.com/raulfrk/herdr-plugin-kit@v0.3.0" ;;
  *" mod download github.com/raulfrk/herdr-plugin-kit@v0.3.0 ")
    : > "$HERDR_TEST_MARKER" ;;
  *" {{.Dir}} "*)
    test -f "$HERDR_TEST_MARKER" || exit 91
    echo "$HERDR_TEST_KIT_DIR" ;;
  *) echo "unexpected fake go invocation: $*" >&2; exit 92 ;;
esac
`
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := `#!/bin/sh
printf '%s\n' "$*" > "$HERDR_TEST_RESULT"
`
	if err := os.WriteFile(filepath.Join(kitDir, "scripts", "mutation-run"), []byte(runner), 0o644); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", filepath.Join(root, "scripts", "mutation-run"), "changed", "verified-base")
	command.Dir = root
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERDR_TEST_KIT_DIR="+kitDir,
		"HERDR_TEST_MARKER="+marker,
		"HERDR_TEST_RESULT="+result,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated wrapper failed: %v\n%s", err, output)
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "changed verified-base\n" {
		t.Fatalf("runner arguments = %q", got)
	}
}

func TestValidateRejectsMissingOrWeakenedMutationContract(t *testing.T) {
	contract, err := generatedMutationContract()
	if err != nil {
		t.Fatal(err)
	}
	weaken := map[string]func([]byte) []byte{
		".gitignore": func(data []byte) []byte {
			return bytes.Replace(data, []byte("/.artifacts/"), []byte("/artifacts/"), 1)
		},
		".gremlins.yaml": func(data []byte) []byte {
			return bytes.Replace(data, []byte("workers: 0"), []byte("workers: 1"), 1)
		},
		"Makefile": func(data []byte) []byte {
			return bytes.Replace(data, []byte("mutation-full:"), []byte("mutation-release:"), 1)
		},
		"docs/mutation-baseline.md": func(data []byte) []byte {
			return bytes.Replace(data, []byte("source SHA-256 is mandatory"), []byte("source SHA-256 is optional"), 1)
		},
		"docs/test-strategy.md": func(data []byte) []byte {
			return bytes.Replace(data, []byte("100% adjusted efficacy"), []byte("99% adjusted efficacy"), 1)
		},
		"mutation-policy.json": func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"adjusted_efficacy": 100`), []byte(`"adjusted_efficacy": 99`), 1)
		},
		"scripts/mutation-run": func(data []byte) []byte {
			return append(data, []byte("# bypass\n")...)
		},
	}
	for name := range contract {
		name := name
		t.Run(name+" missing", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "plugin")
			if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
				t.Fatal(err)
			}
			if err := Validate(root); err == nil {
				t.Fatal("missing mutation contract passed validation")
			}
		})
		t.Run(name+" changed", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "plugin")
			if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(name))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, weaken[name](data), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Validate(root); err == nil {
				t.Fatal("changed mutation contract passed validation")
			}
		})
	}
}

func TestValidateAllowsNonWeakeningMutationContractExtensions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
		t.Fatal(err)
	}
	appendFile := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(text); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendFile(".gitignore", "/local-output/\n")
	appendFile(".gremlins.yaml", "# local explanation\n")
	appendFile("Makefile", "\n.PHONY: test\ntest:\n\tgo test ./...\n")
	appendFile("docs/mutation-baseline.md", "\n")
	appendFile("docs/test-strategy.md", "\n## Local checks\n\nRun project-specific integration checks.\n")

	policyPath := filepath.Join(root, "mutation-policy.json")
	var policy map[string]any
	decodeJSONFile(t, policyPath, &policy)
	policy["critical_paths"] = append(policy["critical_paths"].([]any), "pkg/critical/")
	policy["noncritical"].(map[string]any)["adjusted_efficacy"] = float64(90)
	policy["noncritical"].(map[string]any)["mutant_coverage"] = float64(95)
	writeJSONFile(t, policyPath, policy)
	if err := Validate(root); err != nil {
		t.Fatalf("non-weakening mutation contract extensions rejected: %v", err)
	}
}

func TestValidateRejectsAdditionalProtectedMakeTargetDefinitions(t *testing.T) {
	t.Run("additional target", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin")
		if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "Makefile")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\nmutation-full :\n\t@true\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Validate(root); err == nil {
			t.Fatal("whitespace-variant mutation target override passed validation")
		}
	})
	t.Run("required recipe drift", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin")
		if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "Makefile")
		mutateTextAll(t, path, "GOTOOLCHAIN ?= go1.27.0", "GOTOOLCHAIN?= go1.27.0")
		if err := Validate(root); err == nil {
			t.Fatal("required mutation recipe drift passed validation")
		}
	})
}

func TestValidateOptionalFullMutationBaseline(t *testing.T) {
	newProject := func(t *testing.T) (string, map[string]any) {
		t.Helper()
		root := filepath.Join(t.TempDir(), "plugin")
		if err := Generate(Options{ID: "example.valid", Name: "Valid", Output: root}); err != nil {
			t.Fatal(err)
		}
		config, err := os.ReadFile(filepath.Join(root, ".gremlins.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		policy, err := os.ReadFile(filepath.Join(root, "mutation-policy.json"))
		if err != nil {
			t.Fatal(err)
		}
		return root, map[string]any{
			"schema_version": 1,
			"candidate":      strings.Repeat("a", 40), "report_sha256": strings.Repeat("b", 64),
			"config_sha256": mutation.SHA256(config), "policy_sha256": mutation.SHA256(policy),
			"go_version": runtime.Version(), "gremlins_version": "v0.6.0+hpk.constants.fixture",
			"critical_debt": []any{},
		}
	}
	t.Run("valid empty debt", func(t *testing.T) {
		root, baseline := newProject(t)
		writeJSONFile(t, filepath.Join(root, "mutation-full-baseline.json"), baseline)
		if err := Validate(root); err != nil {
			t.Fatalf("valid full mutation baseline rejected: %v", err)
		}
	})
	for name, mutate := range map[string]func(string, map[string]any){
		"config binding": func(_ string, baseline map[string]any) {
			baseline["config_sha256"] = strings.Repeat("c", 64)
		},
		"policy binding": func(_ string, baseline map[string]any) {
			baseline["policy_sha256"] = strings.Repeat("c", 64)
		},
		"toolchain binding": func(_ string, baseline map[string]any) {
			baseline["go_version"] = "go0.0"
		},
		"source binding": func(_ string, baseline map[string]any) {
			baseline["critical_debt"] = []any{map[string]any{
				"file": "cmd/example.valid/main.go", "line": 1, "column": 1,
				"mutant": "ARITHMETIC_BASE", "status": "LIVED", "owner": "cmd/example.valid",
				"source_sha256": strings.Repeat("d", 64),
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, baseline := newProject(t)
			mutate(root, baseline)
			writeJSONFile(t, filepath.Join(root, "mutation-full-baseline.json"), baseline)
			if err := Validate(root); err == nil {
				t.Fatal("invalid full mutation baseline passed validation")
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		root, _ := newProject(t)
		if err := os.WriteFile(filepath.Join(root, "mutation-full-baseline.json"), []byte("not JSON"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Validate(root); err == nil {
			t.Fatal("malformed full mutation baseline passed validation")
		}
	})
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
	command := exec.Command("go", "test", "-race", "./...")
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

func TestAttentionShapedGeneratedConsumerUsesGenericFixedActionsAndEvents(t *testing.T) {
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
	options := Options{ID: "attention.sessions", Name: "Attention & Sessions", Description: "Fixture-only consumer.", Output: output}
	if err := Generate(options); err != nil {
		t.Fatal(err)
	}

	kitPath := filepath.Join(output, "plugin-kit.json")
	var kit manifest.Manifest
	decodeJSONFile(t, kitPath, &kit)
	extras := []manifest.Action{
		{ID: "open-launcher", Title: "Open launcher", Contexts: []string{"global"}, Command: []string{"./plugin", "action", "open-launcher"}},
		{ID: "toggle-autofocus", Title: "Toggle autofocus", Description: "Toggle fixture behavior", Contexts: []string{"global"}, Command: []string{"./plugin", "action", "toggle-autofocus"}},
		{ID: "observe-event", Title: "Observe event", Contexts: []string{"global"}, Command: []string{"./plugin", "action", "observe-event"}},
	}
	kit.Actions = append(kit.Actions, extras...)
	kit.Events = []manifest.EventHook{
		{On: "pane.focused", Command: []string{"./plugin", "action", "observe-event"}},
		{On: "pane.agent_status_changed", Command: []string{"./plugin", "action", "observe-event"}},
	}
	writeJSONFile(t, kitPath, kit)
	mutateHerdr(t, output, func(value *herdrManifest) {
		for _, action := range extras {
			value.Actions = append(value.Actions, herdrAction{
				ID: action.ID, Title: action.Title, Description: action.Description,
				Contexts: append([]string(nil), action.Contexts...), Command: append([]string(nil), action.Command...),
			})
		}
		for _, event := range kit.Events {
			value.Events = append(value.Events, herdrEvent{On: event.On, Command: append([]string(nil), event.Command...)})
		}
	})
	mainPath := filepath.Join(output, "cmd", options.ID, "main.go")
	mutateText(t, mainPath, `case "health":`, `case "open-launcher", "toggle-autofocus", "observe-event":
		return nil
	case "health":`)

	if err := Validate(output); err != nil {
		t.Fatalf("Attention-shaped fixture validation failed: %v", err)
	}
	workspace := filepath.Join(root, "go.work")
	workspaceData := []byte("go 1.27\n\nuse " + output + "\n\nreplace " + KitModule + " " + KitVersion + " => " + repository + "\n")
	if err := os.WriteFile(workspace, workspaceData, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"test", "./..."}, {"build", "-buildvcs=false", "./cmd/" + options.ID}} {
		command := exec.Command("go", args...)
		command.Dir = output
		command.Env = append(os.Environ(), "GOWORK="+workspace, "GOTOOLCHAIN=go1.27.0")
		if data, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generated %v error = %v\n%s", args, err, data)
		}
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

func TestRollbackPublishedRemovesOnlyThePublishedDirectory(t *testing.T) {
	parent := t.TempDir()
	destinationName := "plugin"
	destination := filepath.Join(parent, destinationName)
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "owned"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	publishedIdentity, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	parentDirectory, parentRoot, err := openStableDirectory(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer parentDirectory.Close()
	defer parentRoot.Close()
	if err := rollbackPublished(parentDirectory, parentRoot, destinationName, publishedIdentity); err != nil {
		t.Fatalf("rollbackPublished() error = %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rollback entries = %v, error = %v", entries, err)
	}
}

func TestStableDirectoryChecksRejectSubstitutedPaths(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real")
	otherPath := filepath.Join(root, "other")
	if err := os.Mkdir(realPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(otherPath, 0o755); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(realPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !sameDirectoryAtPath(realPath, opened) {
		t.Fatal("stable directory was rejected")
	}
	for name, path := range map[string]string{
		"missing": filepath.Join(root, "missing"),
		"file":    filepath.Join(root, "file"),
		"other":   otherPath,
		"symlink": filepath.Join(root, "link"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "file" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "symlink" {
				if err := os.Symlink(realPath, path); err != nil {
					t.Fatal(err)
				}
			}
			if sameDirectoryAtPath(path, opened) {
				t.Fatalf("substituted path %s was accepted", path)
			}
		})
	}
	if _, _, err := openStableDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory was accepted")
	}
	if _, _, err := openStableDirectory(filepath.Join(root, "file")); err == nil {
		t.Fatal("regular file was accepted as a directory")
	}
	if _, _, err := openStableDirectory(filepath.Join(root, "link")); err == nil {
		t.Fatal("symlink was accepted as a stable directory")
	}
}

func TestFileHelpersReportIncompleteWork(t *testing.T) {
	directory := t.TempDir()
	directoryFile, root, err := openStableDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	defer root.Close()
	if err := writeFile(root, "value", []byte("exact")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "value")); err != nil || string(data) != "exact" {
		t.Fatalf("written data = %q, error = %v", data, err)
	}
	if err := writeFile(root, "value", []byte("replacement")); err == nil {
		t.Fatal("exclusive write replaced an existing file")
	}
	if err := syncDirectory(root, ".", "missing"); err == nil {
		t.Fatal("directory sync ignored a missing requested directory")
	}
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
		"action contexts": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["actions"].([]any)[0].(map[string]any)["contexts"] = []string{"global"}
			writeJSONFile(t, path, value)
		},
		"action command": func(t *testing.T, root string) {
			path := filepath.Join(root, "plugin-kit.json")
			var value map[string]any
			decodeJSONFile(t, path, &value)
			value["actions"].([]any)[0].(map[string]any)["command"] = []string{"./plugin", "action", "changed"}
			writeJSONFile(t, path, value)
		},
		"unsupported startup hook": func(t *testing.T, root string) {
			path := filepath.Join(root, "herdr-plugin.toml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("\n[[startup]]\ncommand = [\"./plugin\"]\n")...)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"platform-scoped action": func(t *testing.T, root string) {
			path := filepath.Join(root, "herdr-plugin.toml")
			mutateText(t, path, "contexts = ['global', 'workspace', 'tab', 'pane']", "contexts = ['global', 'workspace', 'tab', 'pane']\nplatforms = ['linux']")
		},
		"platform-scoped event": func(t *testing.T, root string) {
			path := filepath.Join(root, "herdr-plugin.toml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte("\n[[events]]\non = \"pane.focused\"\ncommand = [\"./plugin\"]\nplatforms = [\"linux\"]\n")...)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
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
		"missing generated test": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "cmd", "example.valid", "main_test.go")); err != nil {
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

func TestContractValidatorRejectsEveryIndependentManifestDrift(t *testing.T) {
	type mutation func(*manifest.Manifest, *herdrManifest)
	tests := map[string]mutation{
		"Herdr ID":                 func(_ *manifest.Manifest, h *herdrManifest) { h.ID = "changed" },
		"Herdr name":               func(_ *manifest.Manifest, h *herdrManifest) { h.Name = "changed" },
		"Herdr version":            func(_ *manifest.Manifest, h *herdrManifest) { h.Version = "9.0.0" },
		"Herdr description":        func(_ *manifest.Manifest, h *herdrManifest) { h.Description = "changed" },
		"minimum Herdr":            func(_ *manifest.Manifest, h *herdrManifest) { h.MinHerdrVersion = "9.0.0" },
		"Herdr platform":           func(_ *manifest.Manifest, h *herdrManifest) { h.Platforms = []string{"darwin"} },
		"kit version":              func(k *manifest.Manifest, _ *herdrManifest) { k.Version = "9.0.0" },
		"kit arguments":            func(k *manifest.Manifest, _ *herdrManifest) { k.Args = []string{"extra"} },
		"kit executable":           func(k *manifest.Manifest, _ *herdrManifest) { k.Executable = "other" },
		"build count":              func(_ *manifest.Manifest, h *herdrManifest) { h.Build = nil },
		"build command":            func(_ *manifest.Manifest, h *herdrManifest) { h.Build[0].Command = []string{"other"} },
		"capabilities":             func(k *manifest.Manifest, _ *herdrManifest) { k.Capabilities = []string{"diagnostics"} },
		"kit action count":         func(k *manifest.Manifest, _ *herdrManifest) { k.Actions = k.Actions[:2] },
		"kit extra action":         func(k *manifest.Manifest, _ *herdrManifest) { k.Actions = append(k.Actions, k.Actions[0]) },
		"Herdr action count":       func(_ *manifest.Manifest, h *herdrManifest) { h.Actions = h.Actions[:2] },
		"Herdr extra action":       func(_ *manifest.Manifest, h *herdrManifest) { h.Actions = append(h.Actions, h.Actions[0]) },
		"kit action ID":            func(k *manifest.Manifest, _ *herdrManifest) { k.Actions[0].ID = "changed" },
		"Herdr action ID":          func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[0].ID = "changed" },
		"kit action title":         func(k *manifest.Manifest, _ *herdrManifest) { k.Actions[0].Title = "changed" },
		"Herdr action title":       func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[0].Title = "changed" },
		"kit action description":   func(k *manifest.Manifest, _ *herdrManifest) { k.Actions[0].Description = "changed" },
		"Herdr action description": func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[0].Description = "changed" },
		"action contexts":          func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[0].Contexts = []string{"global"} },
		"action command":           func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[0].Command = []string{"other"} },
		"kit action contexts":      func(k *manifest.Manifest, _ *herdrManifest) { k.Actions[0].Contexts = []string{"global"} },
		"kit action command":       func(k *manifest.Manifest, _ *herdrManifest) { k.Actions[0].Command = []string{"other"} },
		"interface count":          func(k *manifest.Manifest, _ *herdrManifest) { k.Interfaces = nil },
		"extra interface":          func(k *manifest.Manifest, _ *herdrManifest) { k.Interfaces = append(k.Interfaces, k.Interfaces[0]) },
		"interface contract":       func(k *manifest.Manifest, _ *herdrManifest) { k.Interfaces[0].Version = 2 },
		"pane count":               func(_ *manifest.Manifest, h *herdrManifest) { h.Panes = nil },
		"pane ID":                  func(_ *manifest.Manifest, h *herdrManifest) { h.Panes[0].ID = "changed" },
		"pane title":               func(_ *manifest.Manifest, h *herdrManifest) { h.Panes[0].Title = "changed" },
		"pane placement":           func(_ *manifest.Manifest, h *herdrManifest) { h.Panes[0].Placement = "split" },
		"pane command":             func(_ *manifest.Manifest, h *herdrManifest) { h.Panes[0].Command = []string{"other"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := Options{ID: "example.valid", Name: "Valid", Description: "Description"}
			kit := generatedManifest(options)
			herdr := generatedHerdrManifest(options)
			mutate(&kit, &herdr)
			if err := validateContracts(kit, herdr); err == nil {
				t.Fatal("independent manifest drift passed validation")
			}
		})
	}
}

func TestContractValidatorAcceptsAndExactlyMatchesGenericActionsAndEvents(t *testing.T) {
	options := Options{ID: "example.valid", Name: "Valid", Description: "Description"}
	kit := generatedManifest(options)
	herdr := generatedHerdrManifest(options)
	extra := manifest.Action{
		ID: "open-launcher", Title: "Open launcher", Description: "Fixed global action",
		Contexts: []string{"global"}, Command: []string{"./plugin", "action", "open-launcher", "literal $value"},
	}
	kit.Actions = append(kit.Actions, extra)
	herdr.Actions = append(herdr.Actions, herdrAction{
		ID: extra.ID, Title: extra.Title, Description: extra.Description,
		Contexts: append([]string(nil), extra.Contexts...), Command: append([]string(nil), extra.Command...),
	})
	kit.Events = []manifest.EventHook{
		{On: "pane.focused", Command: []string{"./plugin", "action", "observe"}},
		{On: "pane.agent_status_changed", Command: []string{"./plugin", "action", "observe"}},
	}
	herdr.Events = []herdrEvent{
		{On: "pane.focused", Command: []string{"./plugin", "action", "observe"}},
		{On: "pane.agent_status_changed", Command: []string{"./plugin", "action", "observe"}},
	}
	if err := kit.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateContracts(kit, herdr); err != nil {
		t.Fatalf("generic action/event contract rejected: %v", err)
	}

	mutations := map[string]func(*manifest.Manifest, *herdrManifest){
		"extra ID":           func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[3].ID = "changed" },
		"extra metadata":     func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[3].Description = "changed" },
		"extra contexts":     func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[3].Contexts = []string{"global", "pane"} },
		"extra argv":         func(_ *manifest.Manifest, h *herdrManifest) { h.Actions[3].Command[3] = "changed" },
		"extra missing argv": func(k *manifest.Manifest, h *herdrManifest) { k.Actions[3].Command, h.Actions[3].Command = nil, nil },
		"event order":        func(_ *manifest.Manifest, h *herdrManifest) { h.Events[0], h.Events[1] = h.Events[1], h.Events[0] },
		"event argv":         func(_ *manifest.Manifest, h *herdrManifest) { h.Events[0].Command[2] = "changed" },
		"event count":        func(_ *manifest.Manifest, h *herdrManifest) { h.Events = h.Events[:1] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changedKit := kit
			changedKit.Actions = append([]manifest.Action(nil), kit.Actions...)
			changedKit.Events = append([]manifest.EventHook(nil), kit.Events...)
			changedHerdr := herdr
			changedHerdr.Actions = append([]herdrAction(nil), herdr.Actions...)
			changedHerdr.Events = append([]herdrEvent(nil), herdr.Events...)
			for i := range changedHerdr.Actions {
				changedHerdr.Actions[i].Contexts = append([]string(nil), herdr.Actions[i].Contexts...)
				changedHerdr.Actions[i].Command = append([]string(nil), herdr.Actions[i].Command...)
			}
			for i := range changedHerdr.Events {
				changedHerdr.Events[i].Command = append([]string(nil), herdr.Events[i].Command...)
			}
			mutate(&changedKit, &changedHerdr)
			if err := validateContracts(changedKit, changedHerdr); err == nil {
				t.Fatal("generic cross-manifest drift accepted")
			}
		})
	}
}

func TestContractValidatorAcceptsFixedActionRouteAdvertisementTogether(t *testing.T) {
	options := Options{ID: "example.route", Name: "Route"}
	kit := generatedManifest(options)
	herdr := generatedHerdrManifest(options)
	action := manifest.Action{ID: "route", Title: "Route plugin actions", Contexts: []string{"global"}, Command: []string{"./plugin", "action", "route"}}
	kit.Actions = append(kit.Actions, action)
	kit.Interfaces = append(kit.Interfaces, manifest.Interface{ID: "action-route", Version: 1, Direction: manifest.InterfaceProvides})
	herdr.Actions = append(herdr.Actions, herdrAction{ID: action.ID, Title: action.Title, Contexts: append([]string(nil), action.Contexts...), Command: append([]string(nil), action.Command...)})
	if err := validateContracts(kit, herdr); err != nil {
		t.Fatalf("action-route extension rejected: %v", err)
	}
	for name, mutate := range map[string]func(*manifest.Manifest, *herdrManifest){
		"missing interface": func(value *manifest.Manifest, _ *herdrManifest) { value.Interfaces = value.Interfaces[:1] },
		"missing action": func(value *manifest.Manifest, herdrValue *herdrManifest) {
			value.Actions = value.Actions[:3]
			herdrValue.Actions = herdrValue.Actions[:3]
		},
		"wrong title": func(value *manifest.Manifest, herdrValue *herdrManifest) {
			value.Actions[3].Title = "Other"
			herdrValue.Actions[3].Title = "Other"
		},
		"wrong version": func(value *manifest.Manifest, _ *herdrManifest) { value.Interfaces[1].Version = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			changedKit := kit
			changedKit.Actions = append([]manifest.Action(nil), kit.Actions...)
			changedKit.Interfaces = append([]manifest.Interface(nil), kit.Interfaces...)
			changedHerdr := herdr
			changedHerdr.Actions = append([]herdrAction(nil), herdr.Actions...)
			mutate(&changedKit, &changedHerdr)
			if err := validateContracts(changedKit, changedHerdr); err == nil {
				t.Fatal("action-route drift accepted")
			}
		})
	}
}

func TestSourceValidatorRejectsEveryHealthShapeDrift(t *testing.T) {
	validFields := `Version: interop.Version, Interface: "health", InterfaceVersion: 1, Method: "check", Payload: json.RawMessage("{\"ok\":true}")`
	valid := "return interop.ActionResponse{" + validFields + "}"
	tests := map[string]string{
		"missing function":           "",
		"empty body":                 "func healthResponse() any {}",
		"two statements":             "func healthResponse() any { value := 1; " + valid + " }",
		"not return":                 "func healthResponse() any { panic(\"bad\") }",
		"no result":                  "func healthResponse() { return }",
		"two results":                "func healthResponse() (any, any) { return value, value }",
		"not composite":              "func healthResponse() any { return value }",
		"wrong response type":        "func healthResponse() any { return other.Response{" + validFields + "} }",
		"positional field":           "func healthResponse() any { return interop.ActionResponse{interop.Version} }",
		"non-identifier key":         "func healthResponse() any { return interop.ActionResponse{(Version): interop.Version} }",
		"duplicate field":            "func healthResponse() any { return interop.ActionResponse{" + validFields + ", Version: interop.Version} }",
		"missing field":              "func healthResponse() any { return interop.ActionResponse{Version: interop.Version} }",
		"extra field":                "func healthResponse() any { return interop.ActionResponse{" + validFields + ", Extra: 1} }",
		"wrong Version":              "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "interop.Version", "other.Version", 1) + "} }",
		"wrong Version name":         "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "interop.Version", "interop.Other", 1) + "} }",
		"nested Version selector":    "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "interop.Version", "outer.interop.Version", 1) + "} }",
		"bare Version":               "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "interop.Version", "Version", 1) + "} }",
		"wrong Interface":            "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `"health"`, `"other"`, 1) + "} }",
		"non-string Interface":       "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `"health"`, `1`, 1) + "} }",
		"non-literal Interface":      "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `"health"`, `value`, 1) + "} }",
		"wrong interface version":    "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "InterfaceVersion: 1", "InterfaceVersion: 2", 1) + "} }",
		"string interface version":   "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "InterfaceVersion: 1", `InterfaceVersion: "1"`, 1) + "} }",
		"non-literal version":        "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, "InterfaceVersion: 1", "InterfaceVersion: value", 1) + "} }",
		"wrong Method":               "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `"check"`, `"other"`, 1) + "} }",
		"wrong Payload":              "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `true`, `false`, 1) + "} }",
		"Payload not a call":         "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `json.RawMessage("{\"ok\":true}")`, `value`, 1) + "} }",
		"Payload without argument":   "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `json.RawMessage("{\"ok\":true}")`, `json.RawMessage()`, 1) + "} }",
		"Payload with two arguments": "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `json.RawMessage("{\"ok\":true}")`, `json.RawMessage("a", "b")`, 1) + "} }",
		"wrong Payload function":     "func healthResponse() any { return interop.ActionResponse{" + strings.Replace(validFields, `json.RawMessage("{\"ok\":true}")`, `other.RawMessage("{\"ok\":true}")`, 1) + "} }",
	}
	for name, health := range tests {
		t.Run(name, func(t *testing.T) {
			source := "package main\nconst ( pluginID = \"example.valid\"; pluginName = \"Valid\" )\n" + health
			file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
			if err != nil {
				t.Fatalf("test source does not parse: %v\n%s", err, source)
			}
			if err := validateSourceContract(file, "example.valid", "Valid"); err == nil {
				t.Fatal("invalid healthResponse shape passed validation")
			}
		})
	}

	t.Run("duplicate function", func(t *testing.T) {
		source := "package main\nconst ( pluginID = \"example.valid\"; pluginName = \"Valid\" )\n" +
			"func healthResponse() any { " + valid + " }\nfunc healthResponse() any { " + valid + " }"
		file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSourceContract(file, "example.valid", "Valid"); err == nil {
			t.Fatal("duplicate healthResponse passed validation")
		}
	})
}

func TestSourceValidatorRejectsMalformedPluginConstants(t *testing.T) {
	validHealth := `func healthResponse() any { return interop.ActionResponse{Version: interop.Version, Interface: "health", InterfaceVersion: 1, Method: "check", Payload: json.RawMessage("{\"ok\":true}")} }`
	tests := map[string]string{
		"variables instead of constants": `var ( pluginID = "example.valid"; pluginName = "Valid" )`,
		"unequal declaration arity":      `const ( pluginID, pluginName = "example.valid" )`,
		"non-literal ID":                 `const ( other = "example.valid"; pluginID = other; pluginName = "Valid" )`,
		"non-string name":                `const ( pluginID = "example.valid"; pluginName = 1 )`,
		"duplicate ID":                   `const ( pluginID = "example.valid"; pluginID = "example.valid"; pluginName = "Valid" )`,
	}
	for name, declarations := range tests {
		t.Run(name, func(t *testing.T) {
			source := "package main\n" + declarations + "\n" + validHealth
			file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, 0)
			if err != nil {
				t.Fatalf("test source does not parse: %v\n%s", err, source)
			}
			if err := validateSourceContract(file, "example.valid", "Valid"); err == nil {
				t.Fatal("malformed plugin constants passed validation")
			}
		})
	}
}

func TestGoModValidatorRejectsEachIndependentContractDrift(t *testing.T) {
	valid := "module example.com/herdr/example.valid\n\ngo 1.27\n\nrequire " + KitModule + " " + KitVersion + "\n"
	if err := validateGoMod([]byte(valid), "example.valid"); err != nil {
		t.Fatalf("valid go.mod rejected: %v", err)
	}
	tests := map[string]string{
		"missing module":     "go 1.27\n\nrequire " + KitModule + " " + KitVersion + "\n",
		"wrong module":       strings.Replace(valid, "example.valid", "example.other", 1),
		"missing Go version": strings.Replace(valid, "go 1.27\n\n", "", 1),
		"wrong Go version":   strings.Replace(valid, "go 1.27", "go 1.26", 1),
		"wrong kit module":   strings.Replace(valid, KitModule, "example.com/other", 1),
		"wrong kit version":  strings.Replace(valid, KitVersion, "v9.0.0", 1),
		"indirect kit":       strings.Replace(valid, KitVersion, KitVersion+" // indirect", 1),
		"duplicate kit":      valid + "require " + KitModule + " " + KitVersion + "\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateGoMod([]byte(data), "example.valid"); err == nil {
				t.Fatal("invalid go.mod passed validation")
			}
		})
	}
}

func TestReadRegularAcceptsTheLimitAndRejectsLargerOrNonFiles(t *testing.T) {
	directory := t.TempDir()
	directoryFile, root, err := openStableDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	defer root.Close()
	if err := os.WriteFile(filepath.Join(directory, "limit"), bytes.Repeat([]byte("x"), maxFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readRegular(root, "limit"); err != nil || len(data) != maxFileBytes {
		t.Fatalf("limit-sized file length = %d, error = %v", len(data), err)
	}
	if err := os.WriteFile(filepath.Join(directory, "large"), bytes.Repeat([]byte("x"), maxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(root, "large"); err == nil {
		t.Fatal("oversized file passed validation")
	}
	if err := os.Mkdir(filepath.Join(directory, "subdirectory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(root, "subdirectory"); err == nil {
		t.Fatal("directory passed regular-file validation")
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

func TestProductionImportsIncludeFactoredProductionAndExcludeTests(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{
		filepath.Join("cmd", "plugin"), filepath.Join("internal", "feature"), filepath.Join("internal", "unused"),
		filepath.Join("internal", "feature", "testdata"),
	} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join("cmd", "plugin", "main.go"):                      "package main\nimport (\n_ \"" + KitModule + "/config\"\n_ \"example.com/plugin/internal/feature\"\n)\n",
		filepath.Join("internal", "feature", "feature.go"):             "package feature\nimport \"" + KitModule + "/ui/interaction\"\nvar _ = interaction.NewBuffer\n",
		filepath.Join("internal", "feature", "feature_test.go"):        "package feature\nimport _ \"" + KitModule + "/ui/debugui\"\n",
		filepath.Join("internal", "feature", "_ignored.go"):            "package feature\nimport (",
		filepath.Join("internal", "feature", "helper.s"):               "this is not Go source",
		filepath.Join("internal", "feature", "windows.go"):             "//go:build windows\n\npackage feature\nimport (",
		filepath.Join("internal", "feature", "testdata", "fixture.go"): "package fixture\nimport _ \"" + KitModule + "/ui/shell\"\n",
		filepath.Join("internal", "unused", "unused.go"):               "package unused\nimport _ \"" + KitModule + "/ui/theme\"\n",
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	directoryFile, root, err := openStableDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	defer root.Close()
	imports, err := productionImports(root, "cmd/plugin", "example.com/plugin")
	if err != nil {
		t.Fatal(err)
	}
	if !imports[KitModule+"/config"] || !imports[KitModule+"/ui/interaction"] {
		t.Fatalf("production imports = %v", imports)
	}
	if imports[KitModule+"/ui/debugui"] {
		t.Fatalf("test-only import counted as production: %v", imports)
	}
	for _, ignored := range []string{KitModule + "/ui/shell", KitModule + "/ui/theme"} {
		if imports[ignored] {
			t.Fatalf("ignored or unreachable import %q counted as production: %v", ignored, imports)
		}
	}
}

func TestProductionImportsRejectUnsafeSources(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"malformed": func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, "bad.go"), []byte("package bad\nimport ("), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T, directory string) {
			source := append([]byte("//"), bytes.Repeat([]byte("x"), maxFileBytes)...)
			source = append(source, []byte("\npackage large\n")...)
			if err := os.WriteFile(filepath.Join(directory, "large.go"), source, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, directory string) {
			target := filepath.Join(t.TempDir(), "outside.go")
			if err := os.WriteFile(target, []byte("package outside\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(directory, "linked.go")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			prepare(t, directory)
			directoryFile, root, err := openStableDirectory(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer directoryFile.Close()
			defer root.Close()
			if _, err := productionImports(root, ".", "example.com/plugin"); err == nil {
				t.Fatal("unsafe production source passed import collection")
			}
		})
	}

	directory := t.TempDir()
	directoryFile, root, err := openStableDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := productionImports(root, ".", "example.com/plugin"); err == nil {
		t.Fatal("closed production root passed import collection")
	}
}

func TestLocalProductionDirectoryRejectsIgnoredAndInvalidPaths(t *testing.T) {
	const module = "example.com/plugin"
	tests := map[string]struct {
		importPath string
		want       string
		ok         bool
	}{
		"module root":          {importPath: module, want: ".", ok: true},
		"nested production":    {importPath: module + "/internal/feature", want: "internal/feature", ok: true},
		"external module":      {importPath: "example.com/other/internal/feature"},
		"unclean traversal":    {importPath: module + "/internal/../feature"},
		"testdata component":   {importPath: module + "/internal/testdata/feature"},
		"hidden component":     {importPath: module + "/internal/.fixture/feature"},
		"underscore component": {importPath: module + "/internal/_fixture/feature"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := localProductionDirectory(test.importPath, module)
			if got != test.want || ok != test.ok {
				t.Fatalf("localProductionDirectory(%q) = %q, %t; want %q, %t", test.importPath, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestValidateUsesProductionImportsButNotTestOnlyImports(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.factored", Name: "Factored", Output: root}); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(root, "cmd", "example.factored", "main.go")
	mutateText(t, mainPath, KitModule+"/ui/interaction", "example.com/removed/interaction")
	mutateText(t, mainPath, "import (", "import (\n\t_ \"example.com/herdr/example.factored/internal/surface\"")
	directory := filepath.Join(root, "internal", "surface")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	testSource := "package surface\nimport _ \"" + KitModule + "/ui/interaction\"\n"
	if err := os.WriteFile(filepath.Join(directory, "surface_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root); err == nil {
		t.Fatal("required import found only in generated tests passed validation")
	}
	source := "package surface\nimport \"" + KitModule + "/ui/interaction\"\nvar _ = interaction.NewBuffer\n"
	if err := os.WriteFile(filepath.Join(directory, "surface.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root); err != nil {
		t.Fatalf("factored production import rejected: %v", err)
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
