package main

import (
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
	root.AddCommand(newDeployCmd(state))
	root.AddCommand(newDbCmd(state))
	root.AddCommand(newBuildCmd(state))
	root.AddCommand(newLogsCmd(state))
	root.AddCommand(newStatusCmd(state))

	root.AddCommand(newSnapshotCmd(state))
	root.AddCommand(newEnvCmd(state))

	return root
}

// newPlaceholderCmd creates a stub command that prints "not yet implemented".
// These will be replaced with full implementations in subsequent tasks.
func newPlaceholderCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("command %q is not yet implemented — coming soon", use)
		},
	}
}

// getClient loads config (with flag overrides) and returns an *ahclient.Client.
// Commands should call this to get an authenticated client.
func getClient(state *rootState) (*ahclient.Client, error) {
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{})
	if err != nil {
		cfg = &ahclient.Config{}
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
