// Command herdr-plugin-kit generates and validates Herdr Go plugins.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/internal/scaffold"
	"github.com/raulfrk/herdr-plugin-kit/runtime/agenthost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-plugin-kit new|validate|codex-service")
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
	case "codex-service":
		return runCodexService(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type codexServiceCLI interface {
	Install(context.Context, agenthost.CodexServiceInstallOptions) (agenthost.CodexServiceInstallResult, error)
	Inspect(context.Context) (agenthost.CodexServiceSnapshot, error)
	Start(context.Context) (agenthost.CodexServiceSnapshot, error)
	Restart(context.Context, bool) (agenthost.CodexServiceSnapshot, error)
	Uninstall(context.Context, bool) (agenthost.CodexServiceUninstallResult, error)
}

var newCodexServiceCLI = func() codexServiceCLI {
	return agenthost.NewCodexService(command.ExecRunner{})
}

func runCodexService(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: herdr-plugin-kit codex-service install|status|start|restart|uninstall")
	}
	service := newCodexServiceCLI()
	ctx := context.Background()
	switch args[0] {
	case "install":
		flags := flag.NewFlagSet("codex-service install", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var options agenthost.CodexServiceInstallOptions
		flags.StringVar(&options.Executable, "codex", "", "absolute supported Codex executable")
		flags.StringVar(&options.CodexHome, "codex-home", "", "absolute existing CODEX_HOME")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("codex-service install accepts flags only")
		}
		result, err := service.Install(ctx, options)
		if err != nil {
			return err
		}
		if result.Changed {
			if _, err := fmt.Fprintf(stdout, "installed %s and %s\n", result.UnitPath, result.DescriptorPath); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(stdout, "installation unchanged: %s\n", result.UnitPath); err != nil {
				return err
			}
		}
		if result.RestartRequired {
			message := "a staged configuration remains pending; run codex-service restart --confirm-disconnect after settling and saving managed work"
			if result.Changed {
				message = "configuration changed; run codex-service restart --confirm-disconnect after settling and saving managed work"
			}
			_, err = fmt.Fprintln(stdout, message)
			return err
		}
		if result.Changed {
			_, err = fmt.Fprintln(stdout, "run codex-service start to enable and start the shared service")
			return err
		}
		return nil
	case "status":
		if err := flagsOnly("codex-service status", args[1:], stderr); err != nil {
			return err
		}
		snapshot, err := service.Inspect(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "state=%s socket=%s pid=%d invocation=%s\n", snapshot.State, snapshot.SocketPath, snapshot.MainPID, snapshot.InvocationID)
		return err
	case "start":
		if err := flagsOnly("codex-service start", args[1:], stderr); err != nil {
			return err
		}
		snapshot, err := service.Start(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "started shared Codex service pid=%d invocation=%s\n", snapshot.MainPID, snapshot.InvocationID)
		return err
	case "restart":
		confirm, err := disconnectConfirmation("codex-service restart", args[1:], stderr)
		if err != nil {
			return err
		}
		if !confirm {
			return errors.New("restart requires --confirm-disconnect; managed clients will disconnect, so settle and save work first")
		}
		if _, err := fmt.Fprintln(stderr, "warning: managed clients will disconnect; settle and save work before restarting"); err != nil {
			return err
		}
		snapshot, err := service.Restart(ctx, true)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "restarted shared Codex service pid=%d invocation=%s\n", snapshot.MainPID, snapshot.InvocationID)
		return err
	case "uninstall":
		confirm, err := disconnectConfirmation("codex-service uninstall", args[1:], stderr)
		if err != nil {
			return err
		}
		if !confirm {
			return errors.New("uninstall requires --confirm-disconnect; managed clients will disconnect, so settle and save work first")
		}
		if _, err := fmt.Fprintln(stderr, "warning: managed clients will disconnect; settle and save work before uninstalling"); err != nil {
			return err
		}
		result, err := service.Uninstall(ctx, true)
		if err != nil {
			return err
		}
		if result.AlreadyAbsent {
			_, err = fmt.Fprintln(stdout, "Codex service installation is already absent")
			return err
		}
		if result.RemovedUnit {
			if _, err := fmt.Fprintf(stdout, "removed %s\n", result.UnitPath); err != nil {
				return err
			}
		}
		if result.RemovedDescriptor {
			if _, err := fmt.Fprintf(stdout, "removed %s\n", result.DescriptorPath); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(stdout, "retained: %s\n", joinRetained(result.Retained))
		return err
	default:
		return fmt.Errorf("unknown codex-service command %q", args[0])
	}
}

func flagsOnly(name string, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s accepts no arguments", name)
	}
	return nil
}

func disconnectConfirmation(name string, args []string, stderr io.Writer) (bool, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	confirmed := flags.Bool("confirm-disconnect", false, "confirm managed client disconnection")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() != 0 {
		return false, fmt.Errorf("%s accepts flags only", name)
	}
	return *confirmed, nil
}

func joinRetained(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
