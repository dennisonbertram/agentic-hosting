package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// validateRevokeConfirm returns an error if confirm is false, requiring the
// caller to pass --confirm before a destructive key revocation.
func validateRevokeConfirm(confirm bool) error {
	if !confirm {
		return fmt.Errorf("key revocation is permanent; re-run with --confirm to proceed")
	}
	return nil
}

// newKeyCmd builds the `ahc key` command group.
func newKeyCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage API keys",
		Long: `The key command group manages API keys for the current tenant.

Use the subcommands to list, create, or revoke API keys.`,
	}

	cmd.AddCommand(
		newKeyListCmd(root),
		newKeyCreateCmd(root),
		newKeyRevokeCmd(root),
	)

	return cmd
}

// newKeyListCmd builds the `ahc key list` command.
func newKeyListCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all API keys for the current tenant",
		Long: `List all active API keys for the current tenant.

Displays key ID, name, prefix, creation date, and last used date.`,
		Example: `  ahc key list
  ahc key list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}

			keys, err := c.ListKeys()
			if err != nil {
				return fmt.Errorf("list keys failed: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), keys)
			}

			if len(keys) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No API keys found.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tPREFIX\tCREATED\tLAST USED")
			for _, k := range keys {
				lastUsed := "-"
				if k.LastUsedAt != nil {
					lastUsed = time.Unix(*k.LastUsedAt, 0).UTC().Format("2006-01-02")
				}
				created := time.Unix(k.CreatedAt, 0).UTC().Format("2006-01-02")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Prefix, created, lastUsed)
			}
			w.Flush()
			return nil
		},
	}
}

// newKeyCreateCmd builds the `ahc key create` command.
func newKeyCreateCmd(root *rootState) *cobra.Command {
	var (
		nameFlag    string
		expiresFlag string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new API key",
		Long: `Create a new API key for the current tenant.

The raw key is shown exactly once — save it immediately.`,
		Example: `  ahc key create --name ci-bot
  ahc key create --name deploy-key --expires 720h`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}

			req := ahclient.CreateKeyRequest{
				Name: nameFlag,
			}

			if expiresFlag != "" {
				d, err := time.ParseDuration(expiresFlag)
				if err != nil {
					return fmt.Errorf("invalid --expires duration: %w", err)
				}
				secs := int64(d.Seconds())
				req.ExpiresIn = &secs
			}

			result, err := c.CreateKey(req)
			if err != nil {
				return fmt.Errorf("create key failed: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), result)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Key ID : %s\n", result.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Name   : %s\n", result.Name)
			if result.Expires != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", time.Unix(*result.Expires, 0).UTC().Format(time.RFC3339))
			}
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "╔══════════════════════════════════════════╗")
			fmt.Fprintln(cmd.OutOrStdout(), "║              SAVE THIS API KEY           ║")
			fmt.Fprintln(cmd.OutOrStdout(), "║    It will NOT be shown again.           ║")
			fmt.Fprintln(cmd.OutOrStdout(), "╚══════════════════════════════════════════╝")
			fmt.Fprintf(cmd.OutOrStdout(), "\n  API Key: %s\n\n", result.APIKey)
			return nil
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "unnamed", "Key name")
	cmd.Flags().StringVar(&expiresFlag, "expires", "", "Expiry duration (e.g. 720h)")
	return cmd
}

// newKeyRevokeCmd builds the `ahc key revoke` command.
func newKeyRevokeCmd(root *rootState) *cobra.Command {
	var confirmFlag bool

	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API key permanently",
		Long: `Permanently revoke an API key. This action cannot be undone.

The --confirm flag is required as a safety guard.`,
		Example: `  ahc key revoke abc123 --confirm`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRevokeConfirm(confirmFlag); err != nil {
				return err
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			keyID := args[0]
			if err := c.RevokeKey(keyID); err != nil {
				return fmt.Errorf("revoke key failed: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Key %q revoked.\n", keyID)
			return nil
		},
	}

	cmd.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm revocation (required)")
	return cmd
}

// resolveAPIKey returns the API key from the flag, then $AH_API_KEY.
// This helper is used by register_test.go for legacy compatibility testing.
func resolveAPIKey(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("AH_API_KEY"); v != "" {
		return v
	}
	fmt.Fprintln(os.Stderr, "error: API key required; set --key flag or AH_KEY env var")
	os.Exit(1)
	return ""
}
