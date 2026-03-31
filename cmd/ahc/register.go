package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// cliConfig is the structure saved to the config file by --save.
type cliConfig struct {
	APIKey    string `json:"api_key"`
	ServerURL string `json:"server_url"`
}

// saveAPIKey persists the API key and server URL to a JSON config file,
// creating parent directories as needed.
func saveAPIKey(path, apiKey, serverURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	cfg := cliConfig{
		APIKey:    apiKey,
		ServerURL: serverURL,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
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

// defaultConfigPath returns the default path for the ahc config file.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ahc/config.json"
	}
	return filepath.Join(home, ".ahc", "config.json")
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
