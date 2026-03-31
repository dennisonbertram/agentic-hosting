package main

import (
	"fmt"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

func newTenantCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "View tenant info and usage",
		Long: `Display information about your tenant account, including name, email,
status, and current resource usage and quotas.

Examples:
  ahc tenant
  ahc tenant --json
  ahc tenant update --name "New Name"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := getClient(root)
			if err != nil {
				return err
			}

			tenant, err := client.GetTenant()
			if err != nil {
				return fmt.Errorf("failed to get tenant: %w", err)
			}

			usage, err := client.GetTenantUsage()
			if err != nil {
				return fmt.Errorf("failed to get tenant usage: %w", err)
			}

			if root.jsonOutput {
				// Merge tenant and usage into a single object
				type tenantWithUsage struct {
					*ahclient.Tenant
					Usage *ahclient.TenantUsage `json:"usage"`
				}
				return printJSON(cmd.OutOrStdout(), tenantWithUsage{
					Tenant: tenant,
					Usage:  usage,
				})
			}

			// Human-readable output
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, "Tenant Information")
			fmt.Fprintln(w, "------------------")
			fmt.Fprintf(w, "  Name:    %s\n", tenant.Name)
			fmt.Fprintf(w, "  Email:   %s\n", tenant.Email)
			fmt.Fprintf(w, "  Status:  %s\n", tenant.Status)
			fmt.Fprintf(w, "  ID:      %s\n", tenant.ID)
			if tenant.CreatedAt > 0 {
				fmt.Fprintf(w, "  Created: %s\n", time.Unix(tenant.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05 UTC"))
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Resource Usage")
			fmt.Fprintln(w, "--------------")
			fmt.Fprintf(w, "  Services:   %d / %d\n", usage.Services.Used, usage.Services.Max)
			fmt.Fprintf(w, "  Databases:  %d / %d\n", usage.Databases.Used, usage.Databases.Max)
			fmt.Fprintf(w, "  API Keys:   %d / %d\n", usage.APIKeys.Used, usage.APIKeys.Max)
			if usage.MemoryMB > 0 {
				fmt.Fprintf(w, "  Memory:     %d MB\n", usage.MemoryMB)
			}
			if usage.DiskGB > 0 {
				fmt.Fprintf(w, "  Disk:       %d GB\n", usage.DiskGB)
			}
			if usage.RateLimit > 0 {
				fmt.Fprintf(w, "  Rate Limit: %d req/min\n", usage.RateLimit)
			}

			return nil
		},
	}

	cmd.AddCommand(newTenantUpdateCmd(root))
	return cmd
}

func newTenantUpdateCmd(root *rootState) *cobra.Command {
	var nameFlag string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update tenant account settings",
		Long: `Update settings for your tenant account.

Currently supports updating the tenant display name.

Examples:
  ahc tenant update --name "Acme Corp"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nameFlag == "" {
				return fmt.Errorf("no update fields provided — specify at least one flag (e.g. --name)")
			}

			client, err := getClient(root)
			if err != nil {
				return err
			}

			req := ahclient.UpdateTenantRequest{
				Name: &nameFlag,
			}

			tenant, err := client.UpdateTenant(req)
			if err != nil {
				return fmt.Errorf("failed to update tenant: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), tenant)
			}

			printSuccess(cmd.OutOrStdout(), fmt.Sprintf("Tenant updated — name: %s", tenant.Name))
			return nil
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "", "New display name for the tenant")
	return cmd
}
