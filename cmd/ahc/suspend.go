package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSuspendCmd(root *rootState) *cobra.Command {
	var (
		adminToken     string
		bootstrapToken string
		confirmFlag    bool
	)

	cmd := &cobra.Command{
		Use:   "suspend <tenant-id>",
		Short: "Suspend a tenant (operator kill switch)",
		Long: `Suspend a tenant from the operator side: revoke its keys, stop its
workloads, and keep the records for later reactivation.

This is the admin counterpart of DELETE /v1/tenant. Use it when you do not
have a leftover tenant API key.

When the server has AH_ADMIN_TOKEN configured, only the admin token is
accepted: pass it with --admin-token or $AH_ADMIN_TOKEN (sent as
X-Admin-Token). Servers without an admin token accept a bootstrap token
(--bootstrap-token or $AH_BOOTSTRAP_TOKEN).

--confirm is required.`,
		Example: `  AH_ADMIN_TOKEN=<token> ahc suspend <tenant-id> --confirm
  ahc suspend <tenant-id> --admin-token <token> --confirm`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirmFlag {
				return fmt.Errorf("tenant suspension stops all workloads and revokes keys; re-run with --confirm to proceed")
			}
			admin, token := resolveRecoverTokens(adminToken, bootstrapToken)
			if admin == "" && token == "" {
				return fmt.Errorf("admin or bootstrap token required: set --admin-token / $AH_ADMIN_TOKEN, or --bootstrap-token / $AH_BOOTSTRAP_TOKEN")
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			tenantID := args[0]
			if admin != "" {
				err = c.SuspendTenantWithAdminToken(tenantID, admin)
			} else {
				err = c.SuspendTenant(tenantID, token)
			}
			if err != nil {
				return fmt.Errorf("suspend failed: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), map[string]string{
					"tenant_id": tenantID,
					"status":    "suspended",
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Tenant %q suspended. Keys revoked; workloads stopping.\n", tenantID)
			return nil
		},
	}

	cmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin token, sent as X-Admin-Token (overrides $AH_ADMIN_TOKEN). Prefer $AH_ADMIN_TOKEN: this flag exposes the token in shell history and ps")
	cmd.Flags().StringVar(&bootstrapToken, "bootstrap-token", "", "Bootstrap token (overrides $AH_BOOTSTRAP_TOKEN)")
	cmd.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm suspension (required)")
	return cmd
}
