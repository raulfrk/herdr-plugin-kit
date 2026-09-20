package consumerreadiness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const (
	releaseCandidateEnvironment = "HERDR_PLUGIN_KIT_RELEASE_CANDIDATE"
	releaseCandidateModule      = "github.com/raulfrk/herdr-plugin-kit"
	releaseCandidateVersion     = "v0.3.0"
	gremlinsModule              = "github.com/go-gremlins/gremlins"
	gremlinsVersion             = "v0.6.0"
)

func TestUnpublishedV030ProxyConsumers(t *testing.T) {
	if os.Getenv(releaseCandidateEnvironment) != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_RELEASE_CANDIDATE=1 to verify a clean committed v0.3.0 candidate")
	}
	root := releaseRepositoryRoot(t)
	if status := releaseCommand(t, root, nil, "git", "status", "--porcelain"); status != "" {
		t.Fatalf("release candidate worktree is not clean:\n%s", status)
	}
	sha := releaseCommand(t, root, nil, "git", "rev-parse", "HEAD")
	proxy := filepath.Join(t.TempDir(), "proxy")
	toolSeed := filepath.Join(t.TempDir(), "tool-seed")
	writeReleaseFile(t, filepath.Join(toolSeed, "go.mod"), []byte(
		"module example.com/herdr/tool-seed\n\ngo 1.27\n\nrequire "+gremlinsModule+" "+gremlinsVersion+"\n",
	))
	seedDependencyProxy(t, proxy, root, toolSeed)
	publishCandidateToProxy(t, root, proxy)

	for _, consumer := range []struct {
		name        string
		pluginID    string
		attention   bool
		verifyTools bool
	}{
		{name: "default", pluginID: "release-default", verifyTools: true},
		{name: "attention-shaped", pluginID: "attention-shaped", attention: true},
	} {
		consumer := consumer
		t.Run(consumer.name, func(t *testing.T) {
			verifyProxyConsumer(t, proxy, consumer.pluginID, consumer.attention, consumer.verifyTools)
		})
	}
	if t.Failed() {
		return
	}
	t.Logf("verified unpublished %s at candidate SHA %s", releaseCandidateVersion, sha)
}

func releaseRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate release proxy test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), ".."))
}

func seedDependencyProxy(t *testing.T, proxy string, roots ...string) {
	t.Helper()
	moduleCache := releaseCommand(t, roots[0], nil, "go", "env", "GOMODCACHE")
	var queue []module.Version
	selected := make(map[module.Version]bool)
	for _, root := range roots {
		goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err != nil {
			t.Fatalf("read dependencies from %s: %v", root, err)
		}
		parsed, err := modfile.Parse(filepath.Join(root, "go.mod"), goMod, nil)
		if err != nil {
			t.Fatalf("parse dependencies from %s: %v", root, err)
		}
		for _, requirement := range parsed.Require {
			queue = append(queue, requirement.Mod)
			selected[requirement.Mod] = true
		}
	}
	visited := make(map[module.Version]bool)
	versions := make(map[string]map[string]bool)
	for len(queue) > 0 {
		dependency := queue[0]
		queue = queue[1:]
		if visited[dependency] {
			continue
		}
		visited[dependency] = true
		dependencyMod := copyDependencyToProxy(t, moduleCache, proxy, dependency, selected[dependency])
		if len(dependencyMod) == 0 {
			continue
		}
		if versions[dependency.Path] == nil {
			versions[dependency.Path] = make(map[string]bool)
		}
		versions[dependency.Path][dependency.Version] = true
		dependencyFile, parseErr := modfile.Parse(dependency.Path+"@"+dependency.Version+".mod", dependencyMod, nil)
		if parseErr != nil {
			t.Fatalf("parse cached dependency %s@%s: %v", dependency.Path, dependency.Version, parseErr)
		}
		for _, requirement := range dependencyFile.Require {
			queue = append(queue, requirement.Mod)
		}
	}
	for path, found := range versions {
		list := make([]string, 0, len(found))
		for version := range found {
			list = append(list, version)
		}
		sort.Strings(list)
		escapedPath, err := module.EscapePath(path)
		if err != nil {
			t.Fatalf("escape module path %q: %v", path, err)
		}
		writeReleaseFile(t, filepath.Join(proxy, escapedPath, "@v", "list"), []byte(strings.Join(list, "\n")+"\n"))
	}
}

func copyDependencyToProxy(t *testing.T, moduleCache, proxy string, dependency module.Version, selected bool) []byte {
	t.Helper()
	escapedPath, err := module.EscapePath(dependency.Path)
	if err != nil {
		t.Fatalf("escape module path %q: %v", dependency.Path, err)
	}
	escapedVersion, err := module.EscapeVersion(dependency.Version)
	if err != nil {
		t.Fatalf("escape module version %q: %v", dependency.Version, err)
	}
	source := filepath.Join(moduleCache, "cache", "download", escapedPath, "@v")
	destination := filepath.Join(proxy, escapedPath, "@v")
	var goMod []byte
	for _, suffix := range []string{".info", ".mod", ".zip", ".ziphash"} {
		name := escapedVersion + suffix
		data, readErr := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(readErr) && !selected && suffix != ".mod" {
			continue
		}
		if os.IsNotExist(readErr) && !selected {
			return nil
		}
		if os.IsNotExist(readErr) {
			t.Fatalf("cached dependency %s@%s lacks %s; populate the Go cache before running the offline release gate", dependency.Path, dependency.Version, name)
		}
		if readErr != nil {
			t.Fatalf("read cached dependency %s@%s: %v", dependency.Path, dependency.Version, readErr)
		}
		writeReleaseFile(t, filepath.Join(destination, name), data)
		if suffix == ".mod" {
			goMod = data
		}
	}
	return goMod
}

func publishCandidateToProxy(t *testing.T, root, proxy string) {
	t.Helper()
	escapedPath, err := module.EscapePath(releaseCandidateModule)
	if err != nil {
		t.Fatal(err)
	}
	escapedVersion, err := module.EscapeVersion(releaseCandidateVersion)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(proxy, escapedPath, "@v")
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read candidate go.mod: %v", err)
	}
	writeReleaseFile(t, filepath.Join(destination, escapedVersion+".mod"), goMod)
	writeReleaseFile(t, filepath.Join(destination, "list"), []byte(releaseCandidateVersion+"\n"))

	commitTime := releaseCommand(t, root, nil, "git", "show", "-s", "--format=%cI", "HEAD")
	parsedTime, err := time.Parse(time.RFC3339, commitTime)
	if err != nil {
		t.Fatalf("parse candidate commit time %q: %v", commitTime, err)
	}
	info, err := json.Marshal(struct {
		Version string
		Time    time.Time
	}{Version: releaseCandidateVersion, Time: parsedTime})
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseFile(t, filepath.Join(destination, escapedVersion+".info"), append(info, '\n'))

	zipPath := filepath.Join(destination, escapedVersion+".zip")
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseCommand(t, root, nil, "git", "archive", "--format=zip", "--prefix="+releaseCandidateModule+"@"+releaseCandidateVersion+"/", "--output="+zipPath, "HEAD")
}

func verifyProxyConsumer(t *testing.T, proxy, pluginID string, attention, verifyTools bool) {
	t.Helper()
	root := t.TempDir()
	consumer := filepath.Join(root, pluginID)
	moduleCache := filepath.Join(root, "modcache")
	buildCache := filepath.Join(root, "buildcache")
	t.Cleanup(func() { makeReleaseCacheRemovable(t, moduleCache) })
	proxyURL := (&url.URL{Scheme: "file", Path: proxy}).String()
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(root, "home"),
		"GOCACHE=" + buildCache,
		"GOMODCACHE=" + moduleCache,
		"GOPROXY=" + proxyURL,
		"GONOSUMDB=*",
		"GOSUMDB=off",
		"GOFLAGS=-buildvcs=false",
		"GOTOOLCHAIN=go1.27.0",
		"GOWORK=off",
	}
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	releaseCommand(t, root, environment, "go", "run", releaseCandidateModule+"/cmd/herdr-plugin-kit@"+releaseCandidateVersion,
		"new", "--id", pluginID, "--name", pluginID, "--output", consumer)
	if attention {
		writeReleaseFile(t, filepath.Join(consumer, "attention_contract_test.go"), []byte(attentionConsumerContract))
	}
	releaseCommand(t, root, environment, "go", "run", releaseCandidateModule+"/cmd/herdr-plugin-kit@"+releaseCandidateVersion,
		"validate", consumer)
	releaseCommand(t, consumer, environment, "go", "mod", "tidy")
	moduleLine := releaseCommand(t, consumer, environment, "go", "list", "-m", "-f", "{{.Path}} {{.Version}} {{if .Replace}}{{.Replace.Path}}@{{.Replace.Version}}{{else}}<nil>{{end}}", releaseCandidateModule)
	wantModuleLine := releaseCandidateModule + " " + releaseCandidateVersion + " <nil>"
	if moduleLine != wantModuleLine {
		t.Fatalf("candidate module = %q, want %q", moduleLine, wantModuleLine)
	}
	if verifyTools {
		verifyDistributedMutationTooling(t, consumer, environment)
	}
	releaseCommand(t, consumer, environment, "go", "test", "./...")
	releaseCommand(t, consumer, environment, "go", "test", "-race", "./...")
	releaseCommand(t, consumer, environment, "go", "build", "./...")
	goMod, err := os.ReadFile(filepath.Join(consumer, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(goMod, []byte("replace ")) || bytes.Contains(goMod, []byte("replace\n")) {
		t.Fatalf("generated consumer contains a replace directive:\n%s", goMod)
	}
}

func verifyDistributedMutationTooling(t *testing.T, consumer string, environment []string) {
	t.Helper()
	kitDirectory := releaseCommand(t, consumer, environment, "go", "list", "-mod=readonly", "-m", "-f", "{{.Dir}}", releaseCandidateModule)
	for _, name := range []string{
		"scripts/mutation-run", "scripts/gremlins-build", "testtools/gremlins-constant-coverage.patch",
	} {
		info, err := os.Stat(filepath.Join(kitDirectory, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat distributed mutation asset %s: %v", name, err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("distributed mutation asset %s is writable in the module cache", name)
		}
	}
	releaseCommand(t, consumer, environment, "sh", "-n", filepath.Join(kitDirectory, "scripts", "mutation-run"))
	releaseCommand(t, consumer, environment, "sh", "-n", filepath.Join(consumer, "scripts", "mutation-run"))

	patch := filepath.Join(kitDirectory, "testtools", "gremlins-constant-coverage.patch")
	patchHash := strings.Fields(releaseCommand(t, consumer, environment, "sha256sum", patch))[0]
	patchedVersion := gremlinsVersion + "+hpk.constants." + patchHash
	classifier := releaseCandidateModule + "/cmd/mutationgate@" + releaseCandidateVersion

	preflightStart := time.Now()
	releaseCommand(t, consumer, environment, "go", "run", classifier,
		"preflight", "--policy", "mutation-policy.json",
		"--baseline", "docs/mutation-baseline.md", "--config", ".gremlins.yaml",
		"--scope", "full", "--gremlins-version", patchedVersion,
	)
	preflightDuration := time.Since(preflightStart)

	gremlinsBinary := filepath.Join(t.TempDir(), "gremlins")
	toolStart := time.Now()
	releaseCommand(t, consumer, environment, "sh", filepath.Join(kitDirectory, "scripts", "gremlins-build"), gremlinsBinary)
	toolDuration := time.Since(toolStart)
	if version := releaseCommand(t, consumer, environment, gremlinsBinary, "--version"); !strings.Contains(version, gremlinsVersion) {
		t.Fatalf("patched Gremlins version = %q, want it to contain %q", version, gremlinsVersion)
	}

	report := filepath.Join(t.TempDir(), "gremlins.json")
	writeReleaseFile(t, report, []byte(`{"go_module":"example.com/herdr/release-default","files":[{"file_name":"cmd/release-default/main.go","mutations":[{"type":"ARITHMETIC_BASE","status":"KILLED","line":1,"column":1}]}],"test_efficacy":100,"mutations_coverage":100,"mutants_total":1,"mutants_killed":1,"mutants_lived":0,"mutants_not_viable":0,"mutants_not_covered":0,"elapsed_time":1,"mutator_statistics":{"arithmetic_base":1}}`))
	gateReport := filepath.Join(t.TempDir(), "gate.json")
	classificationStart := time.Now()
	releaseCommand(t, consumer, environment, "go", "run", classifier,
		"check", "--report", report, "--policy", "mutation-policy.json",
		"--baseline", "docs/mutation-baseline.md", "--config", ".gremlins.yaml",
		"--candidate", strings.Repeat("a", 40), "--base", strings.Repeat("b", 40),
		"--scope", "changed", "--gremlins-version", patchedVersion, "--output", gateReport,
	)
	classificationDuration := time.Since(classificationStart)
	var result struct {
		Passed bool `json:"passed"`
	}
	data, err := os.ReadFile(gateReport)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode generated-consumer classification: %v", err)
	}
	if !result.Passed {
		t.Fatalf("generated-consumer classification did not pass: %s", data)
	}
	t.Logf("mutation tooling timings: preflight=%s tool_preparation=%s classification=%s", preflightDuration, toolDuration, classificationDuration)
}

func makeReleaseCacheRemovable(t *testing.T, moduleCache string) {
	t.Helper()
	err := filepath.WalkDir(moduleCache, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("make task-owned module cache removable: %v", err)
	}
}

func writeReleaseFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func releaseCommand(t *testing.T, directory string, environment []string, name string, arguments ...string) string {
	t.Helper()
	return strings.TrimSpace(string(releaseCommandBytes(t, directory, environment, name, arguments...)))
}

func releaseCommandBytes(t *testing.T, directory string, environment []string, name string, arguments ...string) []byte {
	t.Helper()
	// Each isolated consumer starts with an empty build cache; race compilation
	// can exceed a minute without indicating a product or performance failure.
	const commandTimeout = 3 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	output, err := command.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%s %s exceeded %s\n%s", name, strings.Join(arguments, " "), commandTimeout, output)
		}
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
	return output
}

const attentionConsumerContract = `package attention_contract

import (
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/agenthost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/interop"
	"github.com/raulfrk/herdr-plugin-kit/runtime/keybindinghost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestAttentionShapedConsumerCompilesSupportedGenericRequests(t *testing.T) {
	origin := sessionhost.Origin{SessionName: "work", WorkspaceID: "workspace", TabID: "tab", PaneID: "pane"}
	prompt, err := agenthost.PreparePrompt("Review the active task without changing its identity.")
	if err != nil {
		t.Fatal(err)
	}
	policy := agenthost.ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium", ApprovalPolicy: "never", SandboxMode: "danger-full-access"}
	threadID := "01a09f5c-37cc-77f1-9409-e9aa070ad8d0"
	source := agenthost.Agent{SessionName: "work", Kind: "codex", Status: "idle", CWD: "/tmp/attention", WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", Session: &agenthost.SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: threadID}}
	managed := []agenthost.ManagedCodexRequest{
		{Operation: agenthost.ManagedNew, Origin: origin, Directory: source.CWD, Placement: sessionhost.NewPane, Direction: "right", Policy: policy, Prompt: &prompt},
		{Operation: agenthost.ManagedFork, Origin: origin, Directory: source.CWD, Placement: sessionhost.NewTab, Source: source, Policy: policy, Prompt: &prompt},
		{Operation: agenthost.ManagedResume, Origin: origin, Directory: source.CWD, ThreadID: threadID, SourceArgv: []string{"codex"}, Policy: policy},
	}
	if managed[0].Placement != sessionhost.NewPane || managed[1].Placement != sessionhost.NewTab || managed[1].Source.Session.Value != threadID || managed[2].Placement != "" || managed[2].ThreadID != threadID {
		t.Fatalf("managed profile topology changed: %+v", managed)
	}
	ordinary := agenthost.PromptStartRequest{Origin: origin, Argv: []string{"custom-agent", "--profile", "review"}, Directory: source.CWD, Placement: sessionhost.NewPane, Direction: "down"}
	if ordinary.Argv[0] != "custom-agent" || ordinary.Placement != sessionhost.NewPane {
		t.Fatalf("ordinary prompt profile changed: %+v", ordinary)
	}
	conversion := agenthost.CodexConversionRequest{Source: source}
	if conversion.Source.PaneID != origin.PaneID {
		t.Fatalf("same-pane conversion source changed: %+v", conversion)
	}
	bindings := keybindinghost.Request{Owner: "attention-shaped", Bindings: []keybindinghost.Binding{
		{Key: "n", Action: "open-new", Description: "Open New profiles"},
		{Key: "f", Action: "open-fork", Description: "Open Fork profiles"},
	}}
	if len(bindings.Bindings) != 2 {
		t.Fatalf("profile bindings changed: %+v", bindings)
	}
	action, contract, err := interop.ActionRouteDeclaration([]string{"./plugin", "action", "route"})
	if err != nil || action.ID != interop.ActionRouteActionID || contract.ID != interop.ActionRouteInterface {
		t.Fatalf("route declaration = %+v, %+v, %v", action, contract, err)
	}
	catalog := interop.RouteCatalog{RouteRevision: "settings-revision", Actions: []interop.RouteAction{
		{ActionID: "new-review", Label: "New review", SuggestedPath: []string{"n", "r"}, Available: true},
		{ActionID: "fork-review", Label: "Fork review", SuggestedPath: []string{"f", "r"}, Available: true},
	}}
	if err := interop.ValidateRouteCatalog(catalog); err != nil {
		t.Fatalf("UI-authored route catalog: %v", err)
	}
}
`
