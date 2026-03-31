package main

import (
	"io"
	"os"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newLogsCmd implements `ahc logs <service-name-or-id> [--tail N] [--follow]`.
func newLogsCmd(root *rootState) *cobra.Command {
	var (
		tail   int
		follow bool
	)

	cmd := &cobra.Command{
		Use:   "logs <service-name-or-id>",
		Short: "Stream service runtime logs",
		Long: `Stream or view the latest runtime logs from a service's container.

Use --tail to control how many recent lines are returned (default 100).
Use --follow to stream logs continuously until Ctrl-C (like 'docker logs -f').

The service can be identified by name or by its 32-character hex ID.`,
		Example: `  ahc logs my-api
  ahc logs my-api --tail 500
  ahc logs my-api --follow
  ahc logs my-api --tail 50 --follow`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runServiceRuntimeLogs(c, args[0], tail, follow, os.Stdout)
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 100, "Number of recent log lines to show")
	cmd.Flags().BoolVar(&follow, "follow", false, "Stream logs continuously (like docker logs -f)")
	return cmd
}

// runServiceRuntimeLogs is the testable core of the logs command.
func runServiceRuntimeLogs(c *ahclient.Client, nameOrID string, tail int, follow bool, w io.Writer) error {
	id, err := resolveServiceID(c, nameOrID)
	if err != nil {
		return err
	}
	reader, err := c.GetServiceLogs(id, follow, tail)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.Copy(w, reader)
	return err
}
