package main

import (
	"fmt"
	"os"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// saveAPIKey persists the API key and server URL to a JSON config file,
// creating parent directories as needed. The file is written in the same
// format that ahclient.LoadConfig reads: {"url": ..., "key": ...}.
func saveAPIKey(path, apiKey, serverURL string) error {
	cfg := &ahclient.Config{
		URL: serverURL,
		Key: apiKey,
	}
	return cfg.Save(path)
}

// resolveBootstrapToken returns the bootstrap token from the flag value (preferred)
// or the named environment variable. Returns an error if neither is available.
func resolveBootstrapToken(flagVal, envVar string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("bootstrap token required: set --%s flag or %s env var", "bootstrap-token", envVar)
}

// defaultConfigPath returns the default path for the ahc config file,
// delegating to ahclient.DefaultConfigPath so both register and configure
// use the same location (~/.ah/config.json).
func defaultConfigPath() string {
	p, err := ahclient.DefaultConfigPath()
	if err != nil {
		return ".ah/config.json"
	}
	return p
}

// newRegisterCmd builds the `ahc register` cobra command.
func newRegisterCmd(root *rootState) *cobra.Command {
	var (
		bootstrapToken string
		saveFlag       bool
	)

	cmd := &cobra.Command{
		Use:   "register <name> <email>",
		Short: "Create a new tenant account",
		Long: `Register a new tenant and obtain an API key.

The API key is shown once — save it immediately.

Requires a bootstrap token, provided by the platform operator.
Set the AH_BOOTSTRAP_TOKEN environment variable or pass --bootstrap-token.`,
		Example: `  # Register using env var for bootstrap token
  AH_BOOTSTRAP_TOKEN=<token> ahc register "Alice" alice@example.com

  # Register and save key to config
  ahc register "Alice" alice@example.com --bootstrap-token <token> --save

  # Register against a custom server
  ahc --url http://localhost:8080 register "Alice" alice@example.com --bootstrap-token <token>`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			email := args[1]

			token, err := resolveBootstrapToken(bootstrapToken, "AH_BOOTSTRAP_TOKEN")
			if err != nil {
				return err
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			result, err := c.Register(name, email, token)
			if err != nil {
				return fmt.Errorf("registration failed: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), result)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Tenant ID : %s\n", result.TenantID)
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "╔══════════════════════════════════════════╗")
			fmt.Fprintln(cmd.OutOrStdout(), "║              SAVE THIS API KEY           ║")
			fmt.Fprintln(cmd.OutOrStdout(), "║    It will NOT be shown again.           ║")
			fmt.Fprintln(cmd.OutOrStdout(), "╚══════════════════════════════════════════╝")
			fmt.Fprintf(cmd.OutOrStdout(), "\n  API Key: %s\n\n", result.APIKey)

			if saveFlag {
				cfgPath := defaultConfigPath()
				serverURL := root.urlFlag
				if serverURL == "" {
					serverURL = os.Getenv("AH_URL")
				}
				if err := saveAPIKey(cfgPath, result.APIKey, serverURL); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save config: %v\n", err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Saved to: %s\n", cfgPath)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&bootstrapToken, "bootstrap-token", "", "Bootstrap token (overrides $AH_BOOTSTRAP_TOKEN)")
	cmd.Flags().BoolVar(&saveFlag, "save", false, "Save API key to ~/.ahc/config.json")
	return cmd
}
