// Package scaffold generates and validates the minimal Herdr Go plugin layout.
package scaffold

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"golang.org/x/mod/modfile"
)

const (
	Version      = "0.1.0"
	KitModule    = "github.com/raulfrk/herdr-plugin-kit"
	KitVersion   = "v0.1.0"
	HerdrVersion = "0.8.2"
	maxFileBytes = 1 << 20
)

//go:embed templates/LICENSE templates/main.go.tmpl templates/main_test.go.tmpl
var templates embed.FS

type Options struct {
	ID          string
	Name        string
	Description string
	Output      string
}

type generationHooks struct {
	beforePublish func()
	syncParent    func(*os.File) error
}

type herdrManifest struct {
	ID              string         `toml:"id"`
	Name            string         `toml:"name"`
	Version         string         `toml:"version"`
	MinHerdrVersion string         `toml:"min_herdr_version"`
	Description     string         `toml:"description,omitempty"`
	Platforms       []string       `toml:"platforms"`
	Build           []herdrCommand `toml:"build"`
	Actions         []herdrAction  `toml:"actions"`
	Panes           []herdrPane    `toml:"panes"`
}

type herdrCommand struct {
	Command []string `toml:"command"`
}

type herdrAction struct {
	ID          string   `toml:"id"`
	Title       string   `toml:"title"`
	Description string   `toml:"description,omitempty"`
	Contexts    []string `toml:"contexts"`
	Command     []string `toml:"command"`
}

type herdrPane struct {
	ID        string   `toml:"id"`
	Title     string   `toml:"title"`
	Placement string   `toml:"placement"`
	Command   []string `toml:"command"`
}

func Generate(options Options) error {
	return generate(options, generationHooks{})
}

func generate(options Options, hooks generationHooks) error {
	kitManifest := generatedManifest(options)
	if err := kitManifest.Validate(); err != nil {
		return fmt.Errorf("validate generated manifest: %w", err)
	}
	if options.Output == "" {
		return errors.New("output directory is empty")
	}
	destination, err := filepath.Abs(options.Output)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create output parent: %w", err)
	}
	parentDirectory, parentRoot, err := openStableDirectory(parent)
	if err != nil {
		return fmt.Errorf("open output parent: %w", err)
	}
	defer parentDirectory.Close()
	defer parentRoot.Close()
	destinationName := filepath.Base(destination)
	if _, err := parentRoot.Lstat(destinationName); err == nil {
		return errors.New("output directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	temporaryName, temporaryRoot, err := createTemporaryRoot(parentRoot, destinationName)
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	published := false
	defer func() {
		_ = temporaryRoot.Close()
		if !published {
			_ = parentRoot.RemoveAll(temporaryName)
		}
	}()
	if err := writeProject(temporaryRoot, options, kitManifest); err != nil {
		return err
	}
	if err := validateOpened(temporaryRoot); err != nil {
		return fmt.Errorf("validate generated project: %w", err)
	}
	publishedIdentity, err := parentRoot.Stat(temporaryName)
	if err != nil {
		return fmt.Errorf("inspect temporary output: %w", err)
	}
	if err := temporaryRoot.Close(); err != nil {
		return fmt.Errorf("close temporary output: %w", err)
	}
	if hooks.beforePublish != nil {
		hooks.beforePublish()
	}
	if !sameDirectoryAtPath(parent, parentDirectory) {
		return errors.New("output parent changed during generation")
	}
	if err := publishNoReplace(parentDirectory, temporaryName, destinationName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("output directory already exists")
		}
		return fmt.Errorf("publish generated project: %w", err)
	}
	published = true
	syncParent := parentDirectory.Sync
	if hooks.syncParent != nil {
		syncParent = func() error { return hooks.syncParent(parentDirectory) }
	}
	if err := syncParent(); err != nil {
		removeErr := rollbackPublished(parentDirectory, parentRoot, destinationName, publishedIdentity)
		resyncErr := parentDirectory.Sync()
		return fmt.Errorf("sync published output (rollback: %v): %w", errors.Join(removeErr, resyncErr), err)
	}
	return nil
}

func rollbackPublished(parentDirectory *os.File, parentRoot *os.Root, destinationName string, publishedIdentity os.FileInfo) error {
	var quarantineName string
	moved := false
	for range 32 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		quarantineName = "." + destinationName + ".rollback-" + hex.EncodeToString(random[:])
		err := publishNoReplace(parentDirectory, destinationName, quarantineName)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("quarantine published output: %w", err)
		}
		moved = true
		break
	}
	if !moved {
		return errors.New("could not allocate rollback quarantine")
	}

	quarantined, err := parentRoot.Stat(quarantineName)
	if err != nil {
		restoreErr := publishNoReplace(parentDirectory, quarantineName, destinationName)
		return fmt.Errorf("inspect rollback quarantine (restore: %v): %w", restoreErr, err)
	}
	if !os.SameFile(quarantined, publishedIdentity) {
		restoreErr := publishNoReplace(parentDirectory, quarantineName, destinationName)
		return fmt.Errorf("published output identity changed; replacement preserved (restore: %v)", restoreErr)
	}
	quarantineRoot, err := parentRoot.OpenRoot(quarantineName)
	if err != nil {
		return fmt.Errorf("open rollback quarantine: %w", err)
	}
	anchored, err := quarantineRoot.Stat(".")
	if err != nil {
		_ = quarantineRoot.Close()
		return fmt.Errorf("inspect rollback quarantine root: %w", err)
	}
	if !os.SameFile(anchored, publishedIdentity) {
		_ = quarantineRoot.Close()
		return errors.New("rollback quarantine identity changed")
	}
	if err := clearRoot(quarantineRoot); err != nil {
		_ = quarantineRoot.Close()
		return fmt.Errorf("clear rollback quarantine: %w", err)
	}
	if err := quarantineRoot.Close(); err != nil {
		return fmt.Errorf("close rollback quarantine: %w", err)
	}
	current, err := parentRoot.Stat(quarantineName)
	if err != nil {
		return fmt.Errorf("inspect rollback quarantine before removal: %w", err)
	}
	if !os.SameFile(current, publishedIdentity) {
		return errors.New("rollback quarantine changed before removal")
	}
	if err := parentRoot.Remove(quarantineName); err != nil {
		return fmt.Errorf("remove rollback quarantine: %w", err)
	}
	return nil
}

func clearRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func sameDirectoryAtPath(path string, opened *os.File) bool {
	current, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if current.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if !current.IsDir() {
		return false
	}
	info, err := opened.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(current, info)
}

func openStableDirectory(path string) (*os.File, *os.Root, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	if current.Mode()&os.ModeSymlink != 0 {
		_ = directory.Close()
		return nil, nil, errors.New("directory must not be a symlink")
	}
	if !current.IsDir() {
		_ = directory.Close()
		return nil, nil, errors.New("path must be a directory")
	}
	if !os.SameFile(opened, current) {
		_ = directory.Close()
		return nil, nil, errors.New("directory must be a stable real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	anchored, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		_ = directory.Close()
		return nil, nil, err
	}
	if !os.SameFile(opened, anchored) {
		_ = root.Close()
		_ = directory.Close()
		return nil, nil, errors.New("directory changed while opening")
	}
	return directory, root, nil
}

func createTemporaryRoot(parent *os.Root, destinationName string) (string, *os.Root, error) {
	for range 32 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := "." + destinationName + ".tmp-" + hex.EncodeToString(random[:])
		if err := parent.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", nil, err
		}
		root, err := parent.OpenRoot(name)
		if err != nil {
			_ = parent.RemoveAll(name)
			return "", nil, err
		}
		return name, root, nil
	}
	return "", nil, errors.New("could not allocate a unique temporary output")
}

func generatedManifest(options Options) manifest.Manifest {
	return manifest.Manifest{
		PluginID: options.ID, Name: options.Name, Version: Version, Description: options.Description,
		Executable:   "./plugin",
		Capabilities: []string{"diagnostics", "responsive-ui"},
		Actions: []manifest.Action{
			{ID: "open", Title: "Open " + options.Name},
			{ID: "debug", Title: "Open " + options.Name + " diagnostics"},
			{ID: "health", Title: "Check " + options.Name + " health"},
		},
		Interfaces: []manifest.Interface{{ID: "health", Version: 1, Direction: manifest.InterfaceProvides}},
	}
}

func writeProject(root *os.Root, options Options, kitManifest manifest.Manifest) error {
	commandDirectory := filepath.Join("cmd", options.ID)
	if err := root.MkdirAll(commandDirectory, 0o755); err != nil {
		return fmt.Errorf("create command directory: %w", err)
	}
	herdr := generatedHerdrManifest(options)
	herdrData, err := toml.Marshal(herdr)
	if err != nil {
		return fmt.Errorf("encode Herdr manifest: %w", err)
	}
	manifestData, err := json.MarshalIndent(kitManifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin kit manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	license, err := templates.ReadFile("templates/LICENSE")
	if err != nil {
		return err
	}
	mainTemplate, err := templates.ReadFile("templates/main.go.tmpl")
	if err != nil {
		return err
	}
	mainData := []byte(strings.NewReplacer(
		"{{PLUGIN_ID}}", strconv.Quote(options.ID),
		"{{PLUGIN_NAME}}", strconv.Quote(options.Name),
	).Replace(string(mainTemplate)))
	mainTestData, err := templates.ReadFile("templates/main_test.go.tmpl")
	if err != nil {
		return err
	}
	readme := "# " + options.Name + "\n\n" + options.Description + "\n\n" +
		"Generated with Herdr Plugin Kit v0.1.0.\n\n" +
		"Build with `go build -o ./plugin ./cmd/" + options.ID + "`, then link this directory with Herdr. " +
		"The generated starter is a deterministic searchable plugin: it demonstrates provider-owned fuzzy ranking, opaque cursor paging, Unicode, stable ties, disabled results, and observable activation. " +
		"The main and diagnostics views are responsive from 40x10 through 500x200; press d to switch views, or Alt+D while editing search text. " +
		"The Debug UI registers semantic previews and exports a bounded owner-only `debug-report.json`; activation writes an owner-only `last-activation.json` receipt under `HERDR_PLUGIN_STATE_DIR`. " +
		"Copy `config.example.toml` to `HERDR_PLUGIN_CONFIG_DIR/config.toml` for local configuration.\n"
	files := map[string][]byte{
		"herdr-plugin.toml":   herdrData,
		"plugin-kit.json":     manifestData,
		"go.mod":              []byte("module example.com/herdr/" + options.ID + "\n\ngo 1.27\n\nrequire " + KitModule + " " + KitVersion + "\n"),
		"LICENSE":             license,
		"README.md":           []byte(readme),
		"config.example.toml": []byte("[theme]\nname = \"catppuccin\"\n"),
		filepath.Join("cmd", options.ID, "main.go"):      mainData,
		filepath.Join("cmd", options.ID, "main_test.go"): mainTestData,
	}
	for name, data := range files {
		if err := writeFile(root, name, data); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return syncDirectory(root, commandDirectory, "cmd", ".")
}

func generatedHerdrManifest(options Options) herdrManifest {
	binary := "./plugin"
	contexts := []string{"global", "workspace", "tab", "pane"}
	return herdrManifest{
		ID: options.ID, Name: options.Name, Version: Version, MinHerdrVersion: HerdrVersion,
		Description: options.Description, Platforms: []string{"linux"},
		Build: []herdrCommand{{Command: []string{"go", "build", "-o", binary, "./cmd/" + options.ID}}},
		Actions: []herdrAction{
			{ID: "open", Title: "Open " + options.Name, Contexts: contexts, Command: []string{binary, "action", "open"}},
			{ID: "debug", Title: "Open " + options.Name + " diagnostics", Contexts: contexts, Command: []string{binary, "action", "debug"}},
			{ID: "health", Title: "Check " + options.Name + " health", Contexts: contexts, Command: []string{binary, "action", "health"}},
		},
		Panes: []herdrPane{
			{ID: "main", Title: options.Name, Placement: "overlay", Command: []string{binary, "ui"}},
		},
	}
}

func writeFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	return errors.Join(writeErr, syncErr, file.Close())
}

func syncDirectory(root *os.Root, names ...string) error {
	for _, name := range names {
		directory, err := root.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
			return err
		}
	}
	return nil
}

func Validate(directory string) error {
	if directory == "" {
		return errors.New("plugin directory is empty")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve plugin directory: %w", err)
	}
	openedDirectory, rootFS, err := openStableDirectory(root)
	if err != nil {
		return fmt.Errorf("open plugin directory: %w", err)
	}
	defer openedDirectory.Close()
	defer rootFS.Close()
	return validateOpened(rootFS)
}

func validateOpened(rootFS *os.Root) error {
	if err := validateTree(rootFS); err != nil {
		return err
	}
	kitData, err := readRegular(rootFS, "plugin-kit.json")
	if err != nil {
		return err
	}
	kitManifest, err := manifest.Parse(bytes.NewReader(kitData))
	if err != nil {
		return fmt.Errorf("plugin-kit.json: %w", err)
	}
	herdrData, err := readRegular(rootFS, "herdr-plugin.toml")
	if err != nil {
		return err
	}
	var herdr herdrManifest
	decoder := toml.NewDecoder(bytes.NewReader(herdrData)).DisallowUnknownFields()
	if err := decoder.Decode(&herdr); err != nil {
		return fmt.Errorf("herdr-plugin.toml: %w", err)
	}
	if err := validateContracts(kitManifest, herdr); err != nil {
		return err
	}
	license, err := readRegular(rootFS, "LICENSE")
	if err != nil {
		return err
	}
	wantLicense, _ := templates.ReadFile("templates/LICENSE")
	if !bytes.Equal(license, wantLicense) {
		return errors.New("LICENSE is not the Apache-2.0 text")
	}
	goMod, err := readRegular(rootFS, "go.mod")
	if err != nil {
		return err
	}
	if err := validateGoMod(goMod, kitManifest.PluginID); err != nil {
		return err
	}
	if _, err := readRegular(rootFS, "README.md"); err != nil {
		return err
	}
	configExample, err := readRegular(rootFS, "config.example.toml")
	if err != nil {
		return err
	}
	if !bytes.Equal(configExample, []byte("[theme]\nname = \"catppuccin\"\n")) {
		return errors.New("config.example.toml differs from the generated theme contract")
	}
	sourceName := filepath.Join("cmd", kitManifest.PluginID, "main.go")
	source, err := readRegular(rootFS, sourceName)
	if err != nil {
		return err
	}
	parsedSource, err := parser.ParseFile(token.NewFileSet(), sourceName, source, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("%s: %w", sourceName, err)
	}
	imports := make(map[string]bool, len(parsedSource.Imports))
	for _, imported := range parsedSource.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err == nil {
			imports[path] = true
		}
	}
	for _, required := range []string{
		KitModule + "/config", KitModule + "/diagnostics", KitModule + "/documentstore", KitModule + "/runtime/interop",
		KitModule + "/ui/debugui", KitModule + "/ui/interaction", KitModule + "/ui/shell", KitModule + "/ui/theme",
	} {
		if !imports[required] {
			return fmt.Errorf("%s omits required import %q", sourceName, required)
		}
	}
	literals := map[string]bool{}
	ast.Inspect(parsedSource, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil {
			literals[value] = true
		}
		return true
	})
	for _, required := range []string{"HERDR_BIN_PATH", "HERDR_PLUGIN_CONFIG_DIR", "HERDR_PLUGIN_STATE_DIR", "ui", "debug", "action"} {
		if !literals[required] {
			return fmt.Errorf("%s omits required contract %q", sourceName, required)
		}
	}
	if err := validateSourceContract(parsedSource, kitManifest.PluginID, kitManifest.Name); err != nil {
		return fmt.Errorf("%s: %w", sourceName, err)
	}
	_, err = readRegular(rootFS, filepath.Join("cmd", kitManifest.PluginID, "main_test.go"))
	return err
}

func validateSourceContract(file *ast.File, pluginID, pluginName string) error {
	constants := map[string]string{}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		if general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if len(value.Names) != len(value.Values) {
				continue
			}
			for index, name := range value.Names {
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok {
					continue
				}
				if literal.Kind != token.STRING {
					continue
				}
				decoded, err := strconv.Unquote(literal.Value)
				if err != nil {
					continue
				}
				if _, exists := constants[name.Name]; exists {
					return fmt.Errorf("constant %s is declared more than once", name.Name)
				}
				constants[name.Name] = decoded
			}
		}
	}
	if constants["pluginID"] != pluginID {
		return errors.New("pluginID differs from the manifests")
	}
	if constants["pluginName"] != pluginName {
		return errors.New("pluginName differs from the manifests")
	}

	var health *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Name.Name != "healthResponse" {
			continue
		}
		if health != nil {
			return errors.New("healthResponse is declared more than once")
		}
		health = function
	}
	if health == nil {
		return errors.New("healthResponse is missing")
	}
	if health.Body == nil {
		return errors.New("healthResponse must have a body")
	}
	if len(health.Body.List) != 1 {
		return errors.New("healthResponse must contain one return statement")
	}
	returned, ok := health.Body.List[0].(*ast.ReturnStmt)
	if !ok {
		return errors.New("healthResponse body must be a return statement")
	}
	if len(returned.Results) != 1 {
		return errors.New("healthResponse must return one action response")
	}
	response, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok {
		return errors.New("healthResponse must return a composite literal")
	}
	if !isSelector(response.Type, "interop", "ActionResponse") {
		return errors.New("healthResponse must return interop.ActionResponse")
	}
	fields := map[string]ast.Expr{}
	for _, element := range response.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return errors.New("healthResponse fields must be unique and named")
		}
		name, named := field.Key.(*ast.Ident)
		if !named {
			return errors.New("healthResponse field names must be identifiers")
		}
		if fields[name.Name] != nil {
			return errors.New("healthResponse fields must be unique and named")
		}
		fields[name.Name] = field.Value
	}
	if len(fields) != 5 {
		return errors.New("healthResponse must contain exactly five fields")
	}
	if !isSelector(fields["Version"], "interop", "Version") {
		return errors.New("healthResponse Version differs from the health interface contract")
	}
	if !isString(fields["Interface"], "health") {
		return errors.New("healthResponse Interface differs from the health interface contract")
	}
	if !isInteger(fields["InterfaceVersion"], "1") {
		return errors.New("healthResponse InterfaceVersion differs from the health interface contract")
	}
	if !isString(fields["Method"], "check") {
		return errors.New("healthResponse Method differs from the health interface contract")
	}
	if !isRawMessage(fields["Payload"], `{"ok":true}`) {
		return errors.New("healthResponse Payload differs from the health interface contract")
	}
	return nil
}

func isSelector(expression ast.Expr, packageName, name string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	identifier, identified := selector.X.(*ast.Ident)
	if !identified {
		return false
	}
	if identifier.Name != packageName {
		return false
	}
	return selector.Sel.Name == name
}

func isString(expression ast.Expr, want string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok {
		return false
	}
	if literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return false
	}
	return value == want
}

func isInteger(expression ast.Expr, want string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok {
		return false
	}
	if literal.Kind != token.INT {
		return false
	}
	return literal.Value == want
}

func isRawMessage(expression ast.Expr, want string) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	if len(call.Args) != 1 {
		return false
	}
	if !isSelector(call.Fun, "json", "RawMessage") {
		return false
	}
	return isString(call.Args[0], want)
}

func validateTree(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must not be a symlink", filepath.ToSlash(path))
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file or directory", filepath.ToSlash(path))
		}
		for _, component := range strings.Split(filepath.ToSlash(path), "/") {
			if component == "catalogue" || strings.HasPrefix(component, "catalogue.") {
				return fmt.Errorf("reserved catalogue content is not allowed: %s", filepath.ToSlash(path))
			}
		}
		return nil
	})
}

func validateContracts(kit manifest.Manifest, herdr herdrManifest) error {
	if herdr.ID != kit.PluginID {
		return errors.New("Herdr and plugin-kit IDs differ")
	}
	if herdr.Name != kit.Name {
		return errors.New("Herdr and plugin-kit names differ")
	}
	if herdr.Version != kit.Version {
		return errors.New("Herdr and plugin-kit versions differ")
	}
	if herdr.Description != kit.Description {
		return errors.New("Herdr and plugin-kit descriptions differ")
	}
	if herdr.MinHerdrVersion != HerdrVersion {
		return errors.New("Herdr manifest must target Herdr 0.8.2")
	}
	if !slices.Equal(herdr.Platforms, []string{"linux"}) {
		return errors.New("Herdr manifest must target Linux")
	}
	binary := kit.Executable
	if kit.Version != Version {
		return errors.New("plugin-kit manifest version differs from the kit version")
	}
	if len(kit.Args) != 0 {
		return errors.New("plugin executable must not have implicit arguments")
	}
	if binary != "./plugin" {
		return errors.New("plugin executable must be ./plugin")
	}
	if len(herdr.Build) != 1 {
		return errors.New("Herdr manifest must declare one build command")
	}
	if !slices.Equal(herdr.Build[0].Command, []string{"go", "build", "-o", binary, "./cmd/" + kit.PluginID}) {
		return errors.New("Herdr build must be the argv-only generated Go build")
	}
	wantActions := []string{"open", "debug", "health"}
	if !slices.Equal(kit.Capabilities, []string{"diagnostics", "responsive-ui"}) {
		return errors.New("generated capability contract differs")
	}
	if len(kit.Actions) != len(wantActions) {
		return errors.New("plugin-kit manifest must declare three actions")
	}
	if len(herdr.Actions) != len(wantActions) {
		return errors.New("Herdr manifest must declare three actions")
	}
	for index, id := range wantActions {
		wantTitle := map[string]string{
			"open": "Open " + kit.Name, "debug": "Open " + kit.Name + " diagnostics", "health": "Check " + kit.Name + " health",
		}[id]
		if kit.Actions[index].ID != id {
			return fmt.Errorf("plugin-kit action %q has the wrong ID", id)
		}
		if herdr.Actions[index].ID != id {
			return fmt.Errorf("Herdr action %q has the wrong ID", id)
		}
		if kit.Actions[index].Title != wantTitle {
			return fmt.Errorf("plugin-kit action %q has the wrong title", id)
		}
		if herdr.Actions[index].Title != wantTitle {
			return fmt.Errorf("Herdr action %q has the wrong title", id)
		}
		if kit.Actions[index].Description != "" {
			return fmt.Errorf("plugin-kit action %q must not have a description", id)
		}
		if herdr.Actions[index].Description != "" {
			return fmt.Errorf("Herdr action %q must not have a description", id)
		}
		if !slices.Equal(herdr.Actions[index].Contexts, []string{"global", "workspace", "tab", "pane"}) {
			return fmt.Errorf("Herdr action %q has the wrong contexts", id)
		}
		if !slices.Equal(herdr.Actions[index].Command, []string{binary, "action", id}) {
			return fmt.Errorf("Herdr action %q has the wrong command", id)
		}
	}
	if len(kit.Interfaces) != 1 {
		return errors.New("generated plugin must provide one interface")
	}
	if kit.Interfaces[0] != (manifest.Interface{ID: "health", Version: 1, Direction: manifest.InterfaceProvides}) {
		return errors.New("generated health interface contract differs")
	}
	if len(herdr.Panes) != 1 {
		return errors.New("Herdr manifest must declare one routed main pane")
	}
	if herdr.Panes[0].ID != "main" {
		return errors.New("main pane must have ID main")
	}
	if herdr.Panes[0].Title != kit.Name {
		return errors.New("main pane title must match the plugin name")
	}
	if herdr.Panes[0].Placement != "overlay" {
		return errors.New("main pane must use overlay placement")
	}
	if !slices.Equal(herdr.Panes[0].Command, []string{binary, "ui"}) {
		return errors.New("main pane must use the routed UI subcommand")
	}
	return nil
}

func validateGoMod(data []byte, pluginID string) error {
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return fmt.Errorf("go.mod: %w", err)
	}
	if len(file.Replace) != 0 {
		return errors.New("go.mod must not contain a replace directive")
	}
	if file.Module == nil {
		return errors.New("go.mod must declare a module")
	}
	if file.Module.Mod.Path != "example.com/herdr/"+pluginID {
		return errors.New("go.mod module identity differs")
	}
	if file.Go == nil {
		return errors.New("go.mod must declare a Go version")
	}
	if file.Go.Version != "1.27" {
		return errors.New("go.mod must require Go 1.27")
	}
	found := 0
	for _, requirement := range file.Require {
		if requirement.Mod.Path != KitModule {
			continue
		}
		if requirement.Mod.Version != KitVersion {
			continue
		}
		if requirement.Indirect {
			continue
		}
		found++
	}
	if found != 1 {
		return fmt.Errorf("go.mod must require %s %s", KitModule, KitVersion)
	}
	return nil
}

func readRegular(root *os.Root, name string) ([]byte, error) {
	file, err := openReadOnlyNoFollow(root, name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, maxFileBytes)
	}
	return data, nil
}
