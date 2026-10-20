package main

import (
	"fmt"
	"os"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newRecoverCmd builds the `ahc recover` cobra command.
func newRecoverCmd(root *rootState) *cobra.Command {
	var (
		emailFlag      string
		bootstrapToken string
		adminToken     string
		saveFlag       bool
	)

	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Recover access using the operator's admin (or bootstrap) token",
		Long: `Recover access to your tenant by generating a new API key. Use this when you
have lost all your API keys.

Recovery needs a token from the platform operator. When the server has
AH_ADMIN_TOKEN configured (the default for new installs), only the admin
token is accepted: pass it with --admin-token or $AH_ADMIN_TOKEN (sent as
X-Admin-Token). Servers without an admin token accept a bootstrap token
(--bootstrap-token or $AH_BOOTSTRAP_TOKEN).

Token precedence: --admin-token, --bootstrap-token, $AH_ADMIN_TOKEN,
$AH_BOOTSTRAP_TOKEN.
The new key is shown exactly once — save it immediately.`,
		Example: `  # Recover with the admin token (servers with AH_ADMIN_TOKEN set)
  AH_ADMIN_TOKEN=<token> ahc recover --email alice@example.com

  # Recover with a bootstrap token (servers without an admin token)
  AH_BOOTSTRAP_TOKEN=<token> ahc recover --email alice@example.com

  # Recover and save to config
  ahc recover --email alice@example.com --bootstrap-token <token> --save`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if emailFlag == "" {
				return fmt.Errorf("--email is required")
			}

			admin, token := resolveRecoverTokens(adminToken, bootstrapToken)
			if admin == "" && token == "" {
				return fmt.Errorf("admin or bootstrap token required: set --admin-token / $AH_ADMIN_TOKEN, or --bootstrap-token / $AH_BOOTSTRAP_TOKEN")
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			var result *ahclient.KeyRecoverResponse
			if admin != "" {
				result, err = c.RecoverKeyWithAdminToken(emailFlag, admin)
			} else {
				result, err = c.RecoverKey(emailFlag, token)
			}
			if err != nil {
				return fmt.Errorf("recovery failed: %w", err)
			}

			if saveFlag {
				saveResolvedConfig(cmd, c, result.Key, root.jsonOutput)
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
			return nil
		},
	}

	cmd.Flags().StringVar(&emailFlag, "email", "", "Registered email address (required)")
	cmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin token, sent as X-Admin-Token (overrides $AH_ADMIN_TOKEN)")
	cmd.Flags().StringVar(&bootstrapToken, "bootstrap-token", "", "Bootstrap token (overrides $AH_BOOTSTRAP_TOKEN)")
	cmd.Flags().BoolVar(&saveFlag, "save", false, "Save recovered API key to ~/.ah/config.json")
	return cmd
}

// resolveRecoverTokens picks the recovery credential: an explicit flag wins
// over the environment, and the admin token over the bootstrap token at the
// same level. It returns (adminToken, "") or ("", bootstrapToken), or two
// empty strings when none is set.
func resolveRecoverTokens(adminFlag, bootstrapFlag string) (admin, bootstrap string) {
	switch {
	case adminFlag != "":
		return adminFlag, ""
	case bootstrapFlag != "":
		return "", bootstrapFlag
	case os.Getenv("AH_ADMIN_TOKEN") != "":
		return os.Getenv("AH_ADMIN_TOKEN"), ""
	default:
		return "", os.Getenv("AH_BOOTSTRAP_TOKEN")
	}
}
