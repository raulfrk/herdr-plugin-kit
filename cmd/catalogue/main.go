// Command catalogue runs the live design review or exports static evidence.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/internal/catalogue"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	return runWithLive(args, stdout, runLive)
}

func runWithLive(args []string, stdout io.Writer, live func() error) error {
	flags := flag.NewFlagSet("catalogue", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "", "empty output directory to create or populate")
	design := flags.String("design", "", "comma-separated design IDs (default all)")
	themeID := flags.String("theme", "", "comma-separated theme IDs (default all)")
	viewport := flags.String("viewport", "", "comma-separated viewport IDs (default all)")
	scenario := flags.String("scenario", "", "comma-separated scenario IDs (default all)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("catalogue accepts flags only")
	}
	if *output == "" {
		if *design != "" || *themeID != "" || *viewport != "" || *scenario != "" {
			return errors.New("export selectors require --output")
		}
		return live()
	}
	manifest, err := catalogue.Export(context.Background(), *output, catalogue.Selection{DesignIDs: split(*design), ThemeIDs: split(*themeID), ViewportIDs: split(*viewport), ScenarioIDs: split(*scenario)})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "catalogue: %d images, %d contact sheets in %s\n", len(manifest.Entries), len(manifest.ContactSheets), *output)
	return err
}

func runLive() error {
	directory, err := catalogueDiagnosticsDirectory()
	if err != nil {
		return err
	}
	recorder, err := diagnostics.Open(diagnostics.DefaultConfig(directory))
	if err != nil {
		return fmt.Errorf("open catalogue diagnostics: %w", err)
	}
	pluginID, _ := diagnostics.NewID("catalogue")
	palette, _ := theme.Builtin("catppuccin")
	program, err := shell.NewProgram(shell.ProgramOptions{
		PluginID: pluginID,
		Theme:    palette,
		Events:   recorder,
	}, catalogue.NewLiveSurface())
	if err != nil {
		return errors.Join(err, recorder.Close())
	}
	_, runErr := program.Run()
	return errors.Join(runErr, recorder.Close())
}

func catalogueDiagnosticsDirectory() (string, error) {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", errors.New("XDG_STATE_HOME must be absolute")
		}
		return filepath.Join(root, "herdr-plugin-kit", "catalogue"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "herdr-plugin-kit", "catalogue"), nil
}

func split(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
