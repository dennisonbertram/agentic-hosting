package main

import (
	"fmt"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// maxMessageLen is the maximum characters to display for a message in table output.
const maxMessageLen = 60

// truncateMessage truncates a message to maxMessageLen and appends "..." if needed.
func truncateMessage(msg string) string {
	if len(msg) <= maxMessageLen {
		return msg
	}
	return msg[:maxMessageLen-3] + "..."
}

// formatActivityTime formats a unix timestamp as a short local time string.
func formatActivityTime(unixSec int64) string {
	if unixSec == 0 {
		return "-"
	}
	t := time.Unix(unixSec, 0).UTC()
	return t.Format("2006-01-02 15:04:05")
}

func newActivityCmd(root *rootState) *cobra.Command {
	var (
		limitFlag int
		sinceFlag string
		typeFlag  string
	)

	cmd := &cobra.Command{
		Use:   "activity",
		Short: "View recent activity events",
		Long: `Display recent activity events from your agentic-hosting instance.

Activity events record operations such as service deployments, starts, stops,
database provisioning, environment creation, and more.

Examples:
  ahc activity
  ahc activity --limit 50
  ahc activity --since 2026-01-01T00:00:00Z
  ahc activity --type service
  ahc activity --type service --limit 20
  ahc activity --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := getClient(root)
			if err != nil {
				return err
			}

			filter := ahclient.ActivityFilter{}

			if limitFlag > 0 {
				filter.Limit = limitFlag
			}

			if sinceFlag != "" {
				t, parseErr := time.Parse(time.RFC3339, sinceFlag)
				if parseErr != nil {
					return fmt.Errorf("invalid --since value %q: must be RFC3339 (e.g. 2026-01-01T00:00:00Z): %w", sinceFlag, parseErr)
				}
				filter.Since = t.Unix()
			}

			if typeFlag != "" {
				filter.ResourceType = typeFlag
			}

			events, err := client.ListActivity(filter)
			if err != nil {
				return fmt.Errorf("failed to list activity: %w", err)
			}

			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), events)
			}

			if len(events) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No activity events found.")
				return nil
			}

			headers := []string{"TIME", "TYPE", "ACTION", "RESOURCE", "MESSAGE"}
			rows := make([][]string, 0, len(events))
			for _, e := range events {
				name := e.ResourceName
				if name == "" {
					name = e.ResourceID
				}
				rows = append(rows, []string{
					formatActivityTime(e.CreatedAt),
					e.ResourceType,
					e.Action,
					name,
					truncateMessage(e.Message),
				})
			}

			printTable(cmd.OutOrStdout(), headers, rows)
			return nil
		},
	}

	cmd.Flags().IntVar(&limitFlag, "limit", 0, "Maximum number of events to return (default: server default)")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "Show events after this time (RFC3339, e.g. 2026-01-01T00:00:00Z)")
	cmd.Flags().StringVar(&typeFlag, "type", "", "Filter by resource type (e.g. service, database, environment)")

	return cmd
}
