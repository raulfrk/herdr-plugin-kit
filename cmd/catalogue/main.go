// Command catalogue exports maintainer-only static UI design studies.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/internal/catalogue"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
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
		return errors.New("--output is required")
	}
	manifest, err := catalogue.Export(context.Background(), *output, catalogue.Selection{DesignIDs: split(*design), ThemeIDs: split(*themeID), ViewportIDs: split(*viewport), ScenarioIDs: split(*scenario)})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "catalogue: %d images, %d contact sheets in %s\n", len(manifest.Entries), len(manifest.ContactSheets), *output)
	return err
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
