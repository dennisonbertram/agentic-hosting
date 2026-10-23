package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// rootState holds global flag values shared across subcommands.
type rootState struct {
	urlFlag    string
	keyFlag    string
	jsonOutput bool
}

// newRootCmd builds and returns the root cobra command.
// It is a constructor so tests can call it multiple times without global state issues.
func newRootCmd() *cobra.Command {
	state := &rootState{}

	root := &cobra.Command{
		Use:   "ahc",
		Short: "ahc — CLI for agentic-hosting",
		Long: `ahc — CLI for agentic-hosting

A command-line tool to deploy, manage, and monitor services on your
agentic-hosting PaaS instance.

Getting Started:
  ahc configure    Set your API URL and key
  ahc register     Create a new tenant account
  ahc deploy       Deploy a service from a git repo or Docker image`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Persistent flags available to all subcommands
	root.PersistentFlags().StringVar(&state.urlFlag, "url", "", "API base URL (overrides config and AH_URL env var)")
	root.PersistentFlags().StringVar(&state.keyFlag, "key", "", "API key (overrides config and AH_KEY env var)")
	root.PersistentFlags().BoolVar(&state.jsonOutput, "json", false, "Output in JSON format")

	// Register subcommands
	root.AddCommand(newVersionCmd(state))
	root.AddCommand(newConfigureCmd(state))

	// Implemented commands
	root.AddCommand(newServiceCmd(state))
	root.AddCommand(newActivityCmd(state))
	root.AddCommand(newTenantCmd(state))
	root.AddCommand(newRegisterCmd(state))
	root.AddCommand(newKeyCmd(state))
	root.AddCommand(newRecoverCmd(state))
	root.AddCommand(newSuspendCmd(state))
	root.AddCommand(newDeployCmd(state))
	root.AddCommand(newDbCmd(state))
	root.AddCommand(newBuildCmd(state))
	root.AddCommand(newLogsCmd(state))
	root.AddCommand(newStatusCmd(state))

	root.AddCommand(newSnapshotCmd(state))
	root.AddCommand(newEnvCmd(state))
	root.AddCommand(newSandboxCmd(state))

	return root
}

// getClient loads config (with flag overrides) and returns an *ahclient.Client.
// Commands should call this to get an authenticated client.
func getClient(state *rootState) (*ahclient.Client, error) {
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{})
	if errors.Is(err, ahclient.ErrNoConfig) {
		cfg = &ahclient.Config{} // flags may still supply everything
	} else if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// Apply flag overrides
	if state.urlFlag != "" {
		cfg.URL = state.urlFlag
	}
	if state.keyFlag != "" {
		cfg.Key = state.keyFlag
	}

	if cfg.URL == "" {
		return nil, fmt.Errorf("no API URL configured — run 'ahc configure --url <URL> --key <KEY>' or set AH_URL")
	}

	return ahclient.NewClient(cfg.URL, cfg.Key), nil
}

// Execute is the main entry point called from main().
func Execute() {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.code) // the command's own output was already shown
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// exitCodeError makes Execute exit with a specific process exit code, used to
// propagate the exit code of a command run by `ahc env exec`.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }
func (e *exitCodeError) ExitCode() int { return e.code }
