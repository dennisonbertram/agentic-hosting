package main

import (
	"fmt"
	"io"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newSnapshotCmd builds the top-level `ahc snapshot` command group.
func newSnapshotCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage service snapshots",
		Long: `The snapshot command group lets you create, list, and delete
point-in-time snapshots of your services.

A snapshot captures the current Docker image and environment variables
of a service so you can roll back if a deploy goes wrong.

Examples:
  ahc snapshot list
  ahc snapshot take my-api
  ahc snapshot take my-api --name before-v2-migration
  ahc snapshot delete <snapshot-id> --confirm`,
	}

	cmd.AddCommand(
		newSnapshotListCmd(root),
		newSnapshotTakeCmd(root),
		newSnapshotDeleteCmd(root),
	)

	return cmd
}

// newSnapshotListCmd implements `ahc snapshot list`.
func newSnapshotListCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all snapshots",
		Long: `List all snapshots for your tenant.

Displays a table with ID (truncated), NAME, SERVICE ID (truncated), and CREATED time.`,
		Example: `  ahc snapshot list
  ahc snapshot list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				snapshots, err := c.ListSnapshots()
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), snapshots)
			}
			return runSnapshotList(c, cmd.OutOrStdout())
		},
	}
}

// runSnapshotList is the testable core of the snapshot list command.
func runSnapshotList(c *ahclient.Client, w io.Writer) error {
	snapshots, err := c.ListSnapshots()
	if err != nil {
		return err
	}

	if len(snapshots) == 0 {
		fmt.Fprintln(w, "No snapshots found.")
		return nil
	}

	headers := []string{"ID", "NAME", "SERVICE", "CREATED"}
	rows := make([][]string, 0, len(snapshots))
	for _, s := range snapshots {
		shortID := s.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		shortSvcID := s.ServiceID
		if len(shortSvcID) > 8 {
			shortSvcID = shortSvcID[:8]
		}
		rows = append(rows, []string{
			shortID,
			s.Name,
			shortSvcID,
			formatTimestamp(s.CreatedAt),
		})
	}
	printTable(w, headers, rows)
	return nil
}

// newSnapshotTakeCmd implements `ahc snapshot take <service-name-or-id> [--name <n>]`.
func newSnapshotTakeCmd(root *rootState) *cobra.Command {
	var nameFlag string

	cmd := &cobra.Command{
		Use:   "take <service-name-or-id>",
		Short: "Take a snapshot of a service",
		Long: `Take a point-in-time snapshot of a service.

The snapshot captures the current Docker image and environment variables.
Use --name to give the snapshot a memorable label; if omitted, the name
defaults to "pre-change-YYYYMMDD".

Examples:
  ahc snapshot take my-api
  ahc snapshot take my-api --name before-v2-migration`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runSnapshotTake(c, cmd.OutOrStdout(), args[0], nameFlag)
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "", `Snapshot name (default: "pre-change-YYYYMMDD")`)
	return cmd
}

// runSnapshotTake is the testable core of the snapshot take command.
func runSnapshotTake(c *ahclient.Client, w io.Writer, nameOrID, name string) error {
	serviceID, err := resolveServiceID(c, nameOrID)
	if err != nil {
		return err
	}

	if name == "" {
		name = "pre-change-" + time.Now().Format("20060102")
	}

	fmt.Fprintf(w, "Taking snapshot %q of service %s...\n", name, nameOrID)

	snap, err := c.CreateSnapshot(serviceID, ahclient.CreateSnapshotRequest{
		Name: name,
	})
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	printSuccess(w, fmt.Sprintf("Snapshot %q created (id: %s)", snap.Name, snap.ID[:8]))
	return nil
}

// newSnapshotDeleteCmd implements `ahc snapshot delete <snapshot-id> --confirm`.
func newSnapshotDeleteCmd(root *rootState) *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "delete <snapshot-id>",
		Short: "Delete a snapshot",
		Long: `Permanently delete a snapshot.

This action is irreversible. You must pass --confirm to proceed.

WARNING: Deleting a snapshot removes the stored image reference. You will
no longer be able to restore the service to this point in time.

Example:
  ahc snapshot delete aabbccdd --confirm`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runSnapshotDelete(c, args[0], confirm)
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false, "Required: confirm you want to permanently delete the snapshot")
	return cmd
}

// runSnapshotDelete is the testable core of the snapshot delete command.
func runSnapshotDelete(c *ahclient.Client, nameOrID string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("refusing to delete without --confirm; re-run with --confirm to proceed")
	}

	id, err := resolveSnapshotID(c, nameOrID)
	if err != nil {
		return err
	}

	return c.DeleteSnapshot(id)
}

// resolveSnapshotID converts a name-or-ID argument into a snapshot ID.
// If arg is exactly 32 lowercase hex characters, it is used directly.
// Otherwise, ListSnapshots is called and filtered by name.
func resolveSnapshotID(c *ahclient.Client, arg string) (string, error) {
	if isHexID(arg) {
		return arg, nil
	}

	snapshots, err := c.ListSnapshots()
	if err != nil {
		return "", fmt.Errorf("list snapshots: %w", err)
	}

	var matches []ahclient.Snapshot
	for _, s := range snapshots {
		if s.Name == arg {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("snapshot %q not found", arg)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("ambiguous: %d snapshots named %q", len(matches), arg)
	}
}
