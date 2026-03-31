package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRecoverCmd builds the `ahc recover` cobra command.
func newRecoverCmd(root *rootState) *cobra.Command {
	var (
		emailFlag      string
		bootstrapToken string
		saveFlag       bool
	)

	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Recover access using a bootstrap token",
		Long: `Recover access to your tenant by generating a new API key using a bootstrap
token. Use this when you have lost all your API keys.

The bootstrap token is provided by the platform operator.
The new key is shown exactly once — save it immediately.`,
		Example: `  # Recover using env var for bootstrap token
  AH_BOOTSTRAP_TOKEN=<token> ahc recover --email alice@example.com

  # Recover and save to config
  ahc recover --email alice@example.com --bootstrap-token <token> --save`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if emailFlag == "" {
				return fmt.Errorf("--email is required")
			}

			token, err := resolveBootstrapToken(bootstrapToken, "AH_BOOTSTRAP_TOKEN")
			if err != nil {
				return err
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			result, err := c.RecoverKey(emailFlag, token)
			if err != nil {
				return fmt.Errorf("recovery failed: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), result)
			}

			if result.Warning != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "WARNING: %s\n", result.Warning)
				if result.RevokedKeyID != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "         Revoked key: %s\n", result.RevokedKeyID)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Key ID : %s\n", result.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Name   : %s\n", result.Name)
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "╔══════════════════════════════════════════╗")
			fmt.Fprintln(cmd.OutOrStdout(), "║           SAVE THIS RECOVERY KEY         ║")
			fmt.Fprintln(cmd.OutOrStdout(), "║    It will NOT be shown again.           ║")
			fmt.Fprintln(cmd.OutOrStdout(), "╚══════════════════════════════════════════╝")
			fmt.Fprintf(cmd.OutOrStdout(), "\n  API Key: %s\n\n", result.Key)

			if saveFlag {
				cfgPath := defaultConfigPath()
				serverURL := root.urlFlag
				if err := saveAPIKey(cfgPath, result.Key, serverURL); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save config: %v\n", err)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Saved to: %s\n", cfgPath)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&emailFlag, "email", "", "Registered email address (required)")
	cmd.Flags().StringVar(&bootstrapToken, "bootstrap-token", "", "Bootstrap token (overrides $AH_BOOTSTRAP_TOKEN)")
	cmd.Flags().BoolVar(&saveFlag, "save", false, "Save recovered API key to ~/.ahc/config.json")
	return cmd
}
