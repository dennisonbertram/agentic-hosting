package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newServiceCmd builds the top-level `ahc service` command group.
func newServiceCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage services on the platform",
		Long: `The service command group lets you create, inspect, and control
services running on the agentic-hosting platform.

Use the subcommands to list services, get details, deploy, start/stop,
manage environment variables, view logs, and review deployment history.`,
	}

	cmd.AddCommand(
		newServiceListCmd(root),
		newServiceGetCmd(root),
		newServiceCreateCmd(root),
		newServiceDeleteCmd(root),
		newServiceStartCmd(root),
		newServiceStopCmd(root),
		newServiceRestartCmd(root),
		newServiceRedeployCmd(root),
		newServiceResetCmd(root),
		newServiceRenameCmd(root),
		newServiceLogsCmd(root),
		newServiceDeploymentsCmd(root),
		newServiceEnvCmd(root),
	)

	return cmd
}

// ---- list ----

func newServiceListCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all services",
		Long: `List all services in your tenant.

Displays a table with the service name, current status, truncated ID,
public URL, and the number of container restarts.`,
		Example: `  ahc service list
  ahc service list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			svcs, err := c.ListServices()
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, svcs)
			}
			writeServiceTable(os.Stdout, svcs)
			return nil
		},
	}
}

// writeServiceTable writes services to w as a formatted table.
func writeServiceTable(w io.Writer, svcs []ahclient.Service) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tID\tURL\tRESTARTS")
	for _, s := range svcs {
		shortID := s.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			s.Name, s.Status, shortID, s.URL, s.CrashCount)
	}
	tw.Flush()
}

// ---- get ----

func newServiceGetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "Get details about a service",
		Long: `Display detailed information about a single service.

Accepts either a service name or a 32-character hex service ID.
When a name is given, all services are fetched and filtered by name.`,
		Example: `  ahc service get my-api
  ahc service get abcdef1234567890abcdef1234567890`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			svc, err := c.GetService(id)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, svc)
			}
			printService(os.Stdout, svc)
			return nil
		},
	}
}

func printService(w io.Writer, s *ahclient.Service) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ID:\t%s\n", s.ID)
	fmt.Fprintf(tw, "Name:\t%s\n", s.Name)
	fmt.Fprintf(tw, "Status:\t%s\n", s.Status)
	fmt.Fprintf(tw, "Image:\t%s\n", s.Image)
	fmt.Fprintf(tw, "Port:\t%d\n", s.Port)
	if s.URL != "" {
		fmt.Fprintf(tw, "URL:\t%s\n", s.URL)
	}
	fmt.Fprintf(tw, "Restarts:\t%d\n", s.CrashCount)
	fmt.Fprintf(tw, "Circuit Open:\t%v\n", s.CircuitOpen)
	if s.LastError != "" {
		fmt.Fprintf(tw, "Last Error:\t%s\n", s.LastError)
	}
	fmt.Fprintf(tw, "Created:\t%s\n", time.Unix(s.CreatedAt, 0).Format(time.RFC3339))
	fmt.Fprintf(tw, "Updated:\t%s\n", time.Unix(s.UpdatedAt, 0).Format(time.RFC3339))
	tw.Flush()
}

// ---- create ----

func newServiceCreateCmd(root *rootState) *cobra.Command {
	var (
		name  string
		image string
		port  int
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new service",
		Long: `Create a new service on the platform.

The service is created with the given name and optional configuration.
After creation, the platform will attempt to deploy the service automatically.

At minimum, a name is required. If --image is not provided, a default
placeholder image is used.`,
		Example: `  ahc service create --name my-api --image nginx:latest --port 80
  ahc service create --name worker --image myapp:v2`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			c, err := getClient(root)
			if err != nil {
				return err
			}
			req := ahclient.CreateServiceRequest{
				Name:  name,
				Image: image,
				Port:  port,
			}
			svc, err := c.CreateService(req)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, svc)
			}
			fmt.Fprintf(os.Stdout, "Service %q created (id: %s, status: %s)\n", svc.Name, svc.ID, svc.Status)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Name for the new service (required)")
	cmd.Flags().StringVar(&image, "image", "", "Docker image to deploy (e.g. nginx:latest)")
	cmd.Flags().IntVar(&port, "port", 0, "Port the service listens on (0 = use default)")
	return cmd
}

// ---- delete ----

func newServiceDeleteCmd(root *rootState) *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete a service",
		Long: `Permanently delete a service and stop all its containers.

This action is irreversible. You must pass --confirm to proceed.
All associated containers are stopped and removed. Env vars and
deployment history are also deleted.`,
		Example: `  ahc service delete my-api --confirm`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runServiceDelete(c, args[0], confirm)
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false, "Required: confirm you want to delete the service")
	return cmd
}

// runServiceDelete is the testable core of the delete command.
func runServiceDelete(c *ahclient.Client, nameOrID string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("refusing to delete without --confirm; re-run with --confirm to proceed")
	}
	id, err := resolveServiceID(c, nameOrID)
	if err != nil {
		return err
	}
	return c.DeleteService(id)
}

// ---- start / stop / restart / redeploy / reset ----

func newServiceStartCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:     "start <name-or-id>",
		Short:   "Start a stopped service",
		Long:    "Start a service that has been stopped or has not yet been deployed.",
		Example: `  ahc service start my-api`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.StartService(id); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Service started.")
			return nil
		},
	}
}

func newServiceStopCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:     "stop <name-or-id>",
		Short:   "Stop a running service",
		Long:    "Stop a running service. The container is terminated gracefully.",
		Example: `  ahc service stop my-api`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.StopService(id); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Service stopped.")
			return nil
		},
	}
}

func newServiceRestartCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:     "restart <name-or-id>",
		Short:   "Restart a service",
		Long:    "Stop and immediately restart a service's container.",
		Example: `  ahc service restart my-api`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.RestartService(id); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Service restarted.")
			return nil
		},
	}
}

func newServiceRedeployCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "redeploy <name-or-id>",
		Short: "Redeploy a service with the current image and env",
		Long: `Redeploy a service by recreating its container using the current image
and environment variables stored in the database.

No new build is triggered. This is useful after updating env vars (via
'ahc service env set') so the changes take effect.`,
		Example: `  ahc service redeploy my-api`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			svc, err := c.RedeployService(id)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, svc)
			}
			fmt.Fprintf(os.Stdout, "Service %q redeployed (status: %s)\n", svc.Name, svc.Status)
			return nil
		},
	}
}

func newServiceResetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "reset <name-or-id>",
		Short: "Reset the circuit breaker for a service",
		Long: `Reset the circuit breaker on a service that has entered a crash loop.

When a service crashes too many times in a short window, the platform
opens the circuit breaker to prevent further restart storms. Use this
command to reset the breaker and allow the service to restart again.`,
		Example: `  ahc service reset my-api`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			if _, err := c.ResetService(id); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Circuit breaker reset.")
			return nil
		},
	}
}

// ---- rename ----

func newServiceRenameCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:     "rename <name-or-id> <new-name>",
		Short:   "Rename a service",
		Long:    "Rename a service. The new name must be unique within your tenant.",
		Example: `  ahc service rename my-api my-new-api`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			svc, err := c.UpdateService(id, args[1])
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, svc)
			}
			fmt.Fprintf(os.Stdout, "Service renamed to %q\n", svc.Name)
			return nil
		},
	}
}

// ---- logs ----

func newServiceLogsCmd(root *rootState) *cobra.Command {
	var (
		tail   int
		follow bool
	)

	cmd := &cobra.Command{
		Use:   "logs <name-or-id>",
		Short: "View runtime logs for a service",
		Long: `Stream or view the latest logs from a service's container.

Use --tail to control how many recent lines are returned (default 100).
Use --follow to stream logs continuously (like 'docker logs -f').`,
		Example: `  ahc service logs my-api
  ahc service logs my-api --tail 500
  ahc service logs my-api --follow`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			reader, err := c.GetServiceLogs(id, follow, tail)
			if err != nil {
				return err
			}
			defer reader.Close()
			_, err = io.Copy(os.Stdout, reader)
			return err
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 100, "Number of recent log lines to show")
	cmd.Flags().BoolVar(&follow, "follow", false, "Stream logs continuously")
	return cmd
}

// ---- deployments ----

func newServiceDeploymentsCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "deployments <name-or-id>",
		Short: "List deployment history for a service",
		Long: `Display the deployment history table for a service.

Each row shows the deployment ID, status, trigger, image, and timestamps.
This is useful for auditing which image versions were deployed and when.`,
		Example: `  ahc service deployments my-api
  ahc service deployments my-api --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			deps, err := c.ListDeployments(id)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, deps)
			}
			writeDeploymentTable(os.Stdout, deps)
			return nil
		},
	}
}

func writeDeploymentTable(w io.Writer, deps []ahclient.Deployment) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTRIGGER\tIMAGE\tSTARTED")
	for _, d := range deps {
		shortID := d.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		shortImage := d.Image
		if len(shortImage) > 40 {
			shortImage = shortImage[:37] + "..."
		}
		started := time.Unix(d.StartedAt, 0).Format("2006-01-02 15:04:05")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			shortID, d.Status, d.Trigger, shortImage, started)
	}
	tw.Flush()
}

// ---- helpers ----

// resolveServiceID converts a name-or-ID argument into a service ID.
//
// Rules:
//   - If arg is exactly 32 lowercase hex characters, it is used directly as an ID.
//   - Otherwise, ListServices is called and the result is filtered by name.
//   - If exactly one match is found, its ID is returned.
//   - If zero matches are found, an error is returned.
//   - If more than one match is found, an "ambiguous" error is returned.
func resolveServiceID(c *ahclient.Client, arg string) (string, error) {
	if isHexID(arg) {
		return arg, nil
	}

	svcs, err := c.ListServices()
	if err != nil {
		return "", fmt.Errorf("list services: %w", err)
	}

	var matches []ahclient.Service
	for _, s := range svcs {
		if s.Name == arg {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("service %q not found", arg)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("ambiguous: %d services named %q", len(matches), arg)
	}
}

// isHexID returns true if s is exactly 32 lowercase hexadecimal characters.
func isHexID(s string) bool {
	if len(s) != 32 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16
}

// parseEnvPairs parses a slice of "KEY=VALUE" strings into a map.
// Values may themselves contain '=' — only the first '=' is the delimiter.
func parseEnvPairs(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		idx := strings.IndexByte(p, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid argument %q: expected KEY=VALUE format", p)
		}
		key := p[:idx]
		value := p[idx+1:]
		if key == "" {
			return nil, fmt.Errorf("empty key in argument %q", p)
		}
		out[key] = value
	}
	return out, nil
}

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// formatTimestamp formats a Unix timestamp as a human-readable string.
// If ts is 0, it returns "-".
func formatTimestamp(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}

// shortStr truncates s to at most n characters, appending "..." if truncated.
func shortStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// pluralize returns word + "s" if count != 1.
func pluralize(count int, word string) string {
	if count == 1 {
		return strconv.Itoa(count) + " " + word
	}
	return strconv.Itoa(count) + " " + word + "s"
}
