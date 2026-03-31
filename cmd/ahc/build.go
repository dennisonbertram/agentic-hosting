package main

import (
	"fmt"
	"io"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newBuildCmd builds the top-level `ahc build` command group.
func newBuildCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "View builds and build logs",
		Long:  `The build command group lets you list builds, stream build logs, and cancel running builds.`,
	}

	cmd.AddCommand(
		newBuildListCmd(root),
		newBuildLogsCmd(root),
		newBuildCancelCmd(root),
	)

	return cmd
}

// newBuildListCmd implements `ahc build list <service-name-or-id> [--all]`.
func newBuildListCmd(root *rootState) *cobra.Command {
	var allFlag bool

	cmd := &cobra.Command{
		Use:   "list [service-name-or-id]",
		Short: "List builds for a service (or all builds with --all)",
		Long: `List builds for a specific service or for the entire tenant.

Without --all, a service name or ID is required. The table shows the
build ID (truncated), status, git URL, branch, and creation time.

Status colors: green = succeeded, red = failed, yellow = building.`,
		Example: `  ahc build list my-api
  ahc build list my-api --json
  ahc build list --all`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			if !allFlag && len(args) == 0 {
				return fmt.Errorf("service name or ID required (or pass --all for tenant-wide list)")
			}
			nameOrID := ""
			if len(args) > 0 {
				nameOrID = args[0]
			}
			return runBuildList(c, nameOrID, allFlag, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVar(&allFlag, "all", false, "List builds for the entire tenant")
	return cmd
}

// runBuildList is the testable core of the build list command.
func runBuildList(c *ahclient.Client, nameOrID string, allFlag bool, w io.Writer) error {
	var builds []ahclient.Build
	var err error

	if allFlag {
		builds, err = c.ListBuilds()
	} else {
		id, resolveErr := resolveServiceID(c, nameOrID)
		if resolveErr != nil {
			return resolveErr
		}
		builds, err = c.ListBuildsForService(id)
	}
	if err != nil {
		return err
	}

	if len(builds) == 0 {
		fmt.Fprintln(w, "No builds found.")
		return nil
	}

	writeBuildTable(w, builds)
	return nil
}

// writeBuildTable writes builds to w as a formatted table.
func writeBuildTable(w io.Writer, builds []ahclient.Build) {
	headers := []string{"ID", "STATUS", "GIT_URL", "BRANCH", "CREATED"}
	rows := make([][]string, 0, len(builds))
	for _, b := range builds {
		shortID := b.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		gitURL := b.SourceURL
		if len(gitURL) > 40 {
			gitURL = gitURL[:37] + "..."
		}
		status := coloredBuildStatus(b.Status)
		rows = append(rows, []string{
			shortID,
			status,
			gitURL,
			b.SourceRef,
			formatTimestamp(b.CreatedAt),
		})
	}
	printTable(w, headers, rows)
}

// coloredBuildStatus returns the status string wrapped in the appropriate ANSI color.
// green = succeeded, red = failed, yellow = building/queued.
func coloredBuildStatus(status string) string {
	switch status {
	case "succeeded":
		return colored(colorGreen, status)
	case "failed":
		return colored(colorRed, status)
	case "building", "queued":
		return colored(colorYellow, status)
	default:
		return status
	}
}

// latestBuildID returns the ID of the first (newest) build in a list.
// The API returns builds ordered newest-first, so builds[0] is the latest.
func latestBuildID(builds []ahclient.Build) (string, error) {
	if len(builds) == 0 {
		return "", fmt.Errorf("no builds found for this service")
	}
	return builds[0].ID, nil
}

// newBuildLogsCmd implements `ahc build logs <service-name-or-id> [build-id]`.
func newBuildLogsCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "logs <service-name-or-id> [build-id]",
		Short: "Stream build logs",
		Long: `Stream build logs for a specific build.

If no build-id is provided, logs for the latest build are streamed.
The connection stays open until the build completes (or Ctrl-C).`,
		Example: `  ahc build logs my-api
  ahc build logs my-api bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}

			serviceID, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}

			var buildID string
			if len(args) == 2 {
				buildID = args[1]
			} else {
				builds, err := c.ListBuildsForService(serviceID)
				if err != nil {
					return err
				}
				buildID, err = latestBuildID(builds)
				if err != nil {
					return err
				}
			}

			reader, err := c.GetBuildLogs(serviceID, buildID, true /* follow */)
			if err != nil {
				return err
			}
			defer reader.Close()
			_, err = io.Copy(cmd.OutOrStdout(), reader)
			return err
		},
	}
}

// newBuildCancelCmd implements `ahc build cancel <service-name-or-id> <build-id>`.
func newBuildCancelCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <service-name-or-id> <build-id>",
		Short: "Cancel a running or queued build",
		Long: `Cancel a build that is currently running or queued.

Both the service name (or ID) and the build ID are required.
The build ID can be obtained from 'ahc build list'.`,
		Example: `  ahc build cancel my-api bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runBuildCancel(c, args[0], args[1])
		},
	}
}

// runBuildCancel is the testable core of the build cancel command.
func runBuildCancel(c *ahclient.Client, nameOrID, buildID string) error {
	serviceID, err := resolveServiceID(c, nameOrID)
	if err != nil {
		return err
	}
	_, err = c.CancelBuild(serviceID, buildID)
	return err
}
