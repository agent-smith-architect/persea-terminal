package main

import (
	"flag"
	"fmt"
	"os"

	"persea-terminal/internal/broker"
	"persea-terminal/internal/config"
	"persea-terminal/internal/frontdoor"
)

var setBrokerDumpability = disableProcessDumpability

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: persea-terminal broker|front [flags]")
	}
	switch args[0] {
	case "broker":
		flags := flag.NewFlagSet("broker", flag.ContinueOnError)
		socket := flags.String("socket", "", "AF_UNIX broker socket path")
		configPath := flags.String("config", "", "strict JSON broker config")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *socket == "" || *configPath == "" || flags.NArg() != 0 {
			return fmt.Errorf("broker requires --socket and --config")
		}
		cfg, err := config.LoadBroker(*configPath)
		if err != nil {
			return err
		}
		if cfg.UnifiedTerminalDev == nil || !cfg.UnifiedTerminalDev.Enabled {
			return fmt.Errorf("broker requires enabled unified_terminal_dev configuration")
		}
		if err := setBrokerDumpability(); err != nil {
			return fmt.Errorf("disable broker dumpability: %w", err)
		}
		effects, err := broker.NewUnifiedDevPaneEffects(cfg)
		if err != nil {
			return err
		}
		return broker.RunWithPaneEffects(*socket, cfg, effects)
	case "front":
		flags := flag.NewFlagSet("front", flag.ContinueOnError)
		configPath := flags.String("config", "", "strict JSON front config")
		staticDir := flags.String("static-dir", "", "clean relative UI bundle directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *configPath == "" || *staticDir == "" || flags.NArg() != 0 {
			return fmt.Errorf("front requires --config and --static-dir")
		}
		cfg, err := config.LoadFront(*configPath)
		if err != nil {
			return err
		}
		return frontdoor.Run(cfg, *staticDir)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}
