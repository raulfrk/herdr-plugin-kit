// Command herdr-plugin-kit generates and validates Herdr Go plugins.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/raulfrk/herdr-plugin-kit/internal/scaffold"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-plugin-kit new|validate")
	}
	switch args[0] {
	case "new":
		flags := flag.NewFlagSet("new", flag.ContinueOnError)
		flags.SetOutput(stderr)
		options := scaffold.Options{}
		flags.StringVar(&options.ID, "id", "", "stable plugin ID")
		flags.StringVar(&options.Name, "name", "", "display name")
		flags.StringVar(&options.Output, "output", "", "new plugin directory")
		flags.StringVar(&options.Description, "description", "", "plugin description")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("new accepts flags only")
		}
		if err := scaffold.Generate(options); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "generated %s\n", options.Output)
		return err
	case "validate":
		flags := flag.NewFlagSet("validate", flag.ContinueOnError)
		flags.SetOutput(stderr)
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: herdr-plugin-kit validate DIR")
		}
		if err := scaffold.Validate(flags.Arg(0)); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "valid %s\n", flags.Arg(0))
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
