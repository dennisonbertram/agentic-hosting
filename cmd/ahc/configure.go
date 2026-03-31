package main

import (
	"fmt"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

func newConfigureCmd(root *rootState) *cobra.Command {
	var (
		urlFlag        string
		keyFlag        string
		configPathFlag string
	)

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Save API connection settings",
		Long: `Save the API URL and key to ~/.ah/config.json.

These values are used by all ahc commands unless overridden with --url / --key
flags or the AH_URL / AH_KEY environment variables.

Examples:
  ahc configure --url https://api.example.com --key ak_...
  ahc configure --url https://localhost:8080 --key ak_dev_...`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// If no flags provided, show current config or guidance
			if urlFlag == "" && keyFlag == "" {
				existing, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: configPathFlag})
				if err != nil {
					fmt.Fprintln(cmd.OutOrStdout(), "No configuration found.")
					fmt.Fprintln(cmd.OutOrStdout(), "")
					fmt.Fprintln(cmd.OutOrStdout(), "To configure ahc, run:")
					fmt.Fprintln(cmd.OutOrStdout(), "  ahc configure --url <API_URL> --key <API_KEY>")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Current configuration:\n")
				fmt.Fprintf(cmd.OutOrStdout(), "  URL: %s\n", existing.URL)
				fmt.Fprintf(cmd.OutOrStdout(), "  Key: %s...\n", maskKey(existing.Key))
				fmt.Fprintln(cmd.OutOrStdout(), "")
				fmt.Fprintln(cmd.OutOrStdout(), "To update, run:")
				fmt.Fprintln(cmd.OutOrStdout(), "  ahc configure --url <API_URL> --key <API_KEY>")
				return nil
			}

			// Determine config path
			cfgPath := configPathFlag
			if cfgPath == "" {
				var err error
				cfgPath, err = ahclient.DefaultConfigPath()
				if err != nil {
					return fmt.Errorf("could not determine config path: %w", err)
				}
			}

			// Load existing or create new config
			cfg := &ahclient.Config{}
			existing, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: cfgPath})
			if err == nil {
				cfg = existing
			}

			// Apply flags
			if urlFlag != "" {
				cfg.URL = urlFlag
			}
			if keyFlag != "" {
				cfg.Key = keyFlag
			}

			// Save
			if err := cfg.Save(cfgPath); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			printSuccess(cmd.OutOrStdout(), fmt.Sprintf("Configuration saved to %s", cfgPath))
			return nil
		},
	}

	cmd.Flags().StringVar(&urlFlag, "url", "", "API base URL (e.g. https://api.example.com)")
	cmd.Flags().StringVar(&keyFlag, "key", "", "API key")
	cmd.Flags().StringVar(&configPathFlag, "config-path", "", "Override config file path (default: ~/.ah/config.json)")

	return cmd
}

// maskKey returns the first 8 chars of the key plus "..." for display.
func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:8]
}
