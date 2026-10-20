package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newSandboxCmd builds the top-level `ahc sandbox` command group (alias sbx).
func newSandboxCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"sbx"},
		Short:   "Disposable sandboxes: run code in a throwaway container with a TTL",
		Long: `The sandbox command group manages disposable sandboxes: isolated containers
that the server deletes when their TTL expires. Use them to run untrusted or
one-off code, or to inspect a copy of a service or snapshot.

By default (--egress none) a sandbox has no network egress: it cannot reach
the internet, other tenant containers or other tenants. With --egress internet,
when the operator has enabled it and your quota allows it, it can reach the
public internet only (not private networks, this host or other sandboxes).
A sandbox cannot be stopped and restarted: when it is done, it is deleted.

Sources (pick one; default --template default):
  --template T     an environment template (see 'ahc env templates')
  --image I        a Docker Hub image, or one of your own built/snapshot images
  --snapshot S     a snapshot of one of your services (runs its app by default)
  --service SVC    the current image of one of your services (runs its app by default)

Quick start:
  # One shot: create, run, delete (exit code propagated)
  ahc sandbox run --image python:3.12-slim -- python3 -c "print(2+2)"

  # Keep one around for a while
  ahc sandbox create scratch --template default --ttl 1h
  ahc sandbox exec scratch -- ls /workspace
  ahc sandbox extend scratch --ttl 2h
  ahc sandbox delete scratch

Full command reference:
  ahc sandbox create [name] [source] [--ttl 30m] [--run idle|app] [--include-env] [--memory MB] [--cpu MILLI]
                      [--disk MB] [--egress none|internet]
  ahc sandbox list [--status S]
  ahc sandbox get <name-or-id>
  ahc sandbox exec <name-or-id> [--workdir D] [--timeout S] -- <cmd...>
  ahc sandbox logs <name-or-id> [--tail N] [-f]
  ahc sandbox extend <name-or-id> --ttl 1h
  ahc sandbox delete <name-or-id>
  ahc sandbox run [source] [--ttl 10m] [--workdir D] [--timeout S] -- <cmd...>`,
	}
	cmd.AddCommand(
		newSandboxCreateCmd(root),
		newSandboxListCmd(root),
		newSandboxGetCmd(root),
		newSandboxExecCmd(root),
		newSandboxLogsCmd(root),
		newSandboxExtendCmd(root),
		newSandboxDeleteCmd(root),
		newSandboxRunCmd(root),
	)
	return cmd
}

// sandboxCreateOpts are the flags shared by `sandbox create` and `sandbox run`.
type sandboxCreateOpts struct {
	name       string
	template   string
	image      string
	snapshot   string
	service    string
	ttl        string
	run        string
	includeEnv bool
	memoryMB   int
	cpuMilli   int
	diskMB     int
	egress     string
}

func addSandboxSourceFlags(cmd *cobra.Command, o *sandboxCreateOpts, defaultTTL string) {
	f := cmd.Flags()
	f.StringVar(&o.template, "template", "", "Environment template name or ID (default when no source is given: default)")
	f.StringVar(&o.image, "image", "", "Image: Docker Hub, or your own 127.0.0.1:5000 build or snapshot image")
	f.StringVar(&o.snapshot, "snapshot", "", "Snapshot ID to start from (runs its app by default)")
	f.StringVar(&o.service, "service", "", "Service ID to fork (uses its current image; runs its app by default)")
	f.StringVar(&o.ttl, "ttl", defaultTTL, "Time to live, as a duration (30m, 2h) or seconds")
	f.StringVar(&o.run, "run", "", "idle (sleep, exec into it) or app (image's own command); default depends on the source")
	f.BoolVar(&o.includeEnv, "include-env", false, "Copy the snapshot's or service's env vars into the sandbox (values never shown)")
	f.IntVar(&o.memoryMB, "memory", 0, "Memory limit in MB (default: template value or 512)")
	f.IntVar(&o.cpuMilli, "cpu", 0, "CPU limit in millicores (default: template value or 1000)")
	f.IntVar(&o.diskMB, "disk", 0, "Workspace size in MB, at most --memory (only on servers with tmpfs workspaces)")
	f.StringVar(&o.egress, "egress", "", "none (default) or internet (only if the operator and your quota allow it)")
}

// parseTTL accepts a Go duration ("30m", "2h") or whole seconds ("1800").
func parseTTL(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("--ttl must be positive")
		}
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--ttl %q: use a duration such as 30m or 2h, or seconds", s)
	}
	if d < time.Second {
		return 0, fmt.Errorf("--ttl must be at least 1s")
	}
	return int(d / time.Second), nil
}

// buildSandboxRequest maps flags to an API request.
func buildSandboxRequest(o sandboxCreateOpts) (ahclient.CreateSandboxRequest, error) {
	req := ahclient.CreateSandboxRequest{Name: o.name, Run: o.run, IncludeEnv: o.includeEnv, Egress: o.egress}
	n := 0
	if o.template != "" {
		n++
		req.Source = ahclient.SandboxSource{Type: "template", Template: o.template}
	}
	if o.image != "" {
		n++
		req.Source = ahclient.SandboxSource{Type: "image", Image: o.image}
	}
	if o.snapshot != "" {
		n++
		req.Source = ahclient.SandboxSource{Type: "snapshot", SnapshotID: o.snapshot}
	}
	if o.service != "" {
		n++
		req.Source = ahclient.SandboxSource{Type: "service", ServiceID: o.service}
	}
	switch {
	case n > 1:
		return req, fmt.Errorf("use only one of --template, --image, --snapshot, --service")
	case n == 0:
		req.Source = ahclient.SandboxSource{Type: "template", Template: "default"}
	}
	ttl, err := parseTTL(o.ttl)
	if err != nil {
		return req, err
	}
	if ttl > 0 {
		req.TTLSeconds = &ttl
	}
	if o.memoryMB > 0 {
		m := o.memoryMB
		req.MemoryMB = &m
	}
	if o.cpuMilli > 0 {
		c := o.cpuMilli
		req.CPUMillicores = &c
	}
	if o.diskMB > 0 {
		d := o.diskMB
		req.DiskMB = &d
	}
	return req, nil
}

func newSandboxCreateCmd(root *rootState) *cobra.Command {
	var o sandboxCreateOpts
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a sandbox",
		Example: `  ahc sandbox create
  ahc sandbox create scratch --image python:3.12-slim --ttl 1h
  ahc sandbox create web-copy --service <service-id> --include-env
  ahc sandbox create --snapshot <snapshot-id> --run idle --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.name = args[0]
			}
			req, err := buildSandboxRequest(o)
			if err != nil {
				return err
			}
			c, err := getClient(root)
			if err != nil {
				return err
			}
			s, err := c.CreateSandbox(req)
			if err != nil {
				return fmt.Errorf("create sandbox: %w", err)
			}
			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), s)
			}
			w := cmd.OutOrStdout()
			printSuccess(w, fmt.Sprintf("Sandbox %q created (id: %s, expires: %s)", s.Name, shortID(s.ID), formatUnix(s.ExpiresAt)))
			fmt.Fprintf(w, "\nRun a command:\n  ahc sandbox exec %s -- <command>\n", s.Name)
			return nil
		},
	}
	addSandboxSourceFlags(cmd, &o, "")
	return cmd
}

func formatUnix(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func newSandboxListCmd(root *rootState) *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List sandboxes",
		Example: "  ahc sandbox list\n  ahc sandbox list --status running --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			list, err := c.ListSandboxes(status)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), list)
			}
			return printSandboxTable(cmd.OutOrStdout(), list)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (creating, running, stopped, failed, deleting)")
	return cmd
}

func printSandboxTable(w io.Writer, list []ahclient.Sandbox) error {
	if len(list) == 0 {
		fmt.Fprintln(w, "No sandboxes found.")
		return nil
	}
	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{s.Name, s.Status, s.Source.Type + ":" + shortStr(s.Source.Ref, 24), s.Run, shortID(s.ID), formatUnix(s.ExpiresAt)})
	}
	printTable(w, []string{"NAME", "STATUS", "SOURCE", "RUN", "ID", "EXPIRES"}, rows)
	return nil
}

func newSandboxGetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "Show a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveSandboxID(c, args[0])
			if err != nil {
				return err
			}
			s, err := c.GetSandbox(id)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), s)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "ID:        %s\n", s.ID)
			fmt.Fprintf(w, "Name:      %s\n", s.Name)
			fmt.Fprintf(w, "Status:    %s\n", s.Status)
			fmt.Fprintf(w, "Source:    %s %s\n", s.Source.Type, s.Source.Ref)
			fmt.Fprintf(w, "Image:     %s\n", s.Image)
			fmt.Fprintf(w, "Run:       %s\n", s.Run)
			fmt.Fprintf(w, "Egress:    %s\n", s.Egress)
			fmt.Fprintf(w, "Resources: %d MB, %d millicores\n", s.MemoryMB, s.CPUMillicores)
			if s.DiskMB > 0 {
				fmt.Fprintf(w, "Workspace: %d MB (tmpfs, counts toward memory)\n", s.DiskMB)
			}
			fmt.Fprintf(w, "Expires:   %s (max %s)\n", formatUnix(s.ExpiresAt), formatUnix(s.MaxExpiresAt))
			fmt.Fprintf(w, "Created:   %s\n", formatUnix(s.CreatedAt))
			return nil
		},
	}
}

func newSandboxExecCmd(root *rootState) *cobra.Command {
	var workdir string
	var timeout int
	cmd := &cobra.Command{
		Use:   "exec <name-or-id> -- <command...>",
		Short: "Run a command in a sandbox (exit code propagated)",
		Long: `Run a command (argv, no shell) in a running sandbox and print its output.
Wrap pipelines yourself: ahc sandbox exec s -- sh -c 'ls | wc -l'.
The image must provide the coreutils 'timeout' binary.`,
		Example: `  ahc sandbox exec scratch -- python3 -V
  ahc sandbox exec scratch --workdir /app --timeout 120 -- npm test`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("no command given: ahc sandbox exec <name-or-id> -- <command...>")
			}
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveSandboxID(c, args[0])
			if err != nil {
				return err
			}
			return runSandboxExec(c, cmd.OutOrStdout(), cmd.ErrOrStderr(), id, args[1:], workdir, timeout)
		},
	}
	cmd.Flags().StringVar(&workdir, "workdir", "", "Working directory (default /workspace)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Timeout in seconds (default 60, max 300)")
	return cmd
}

// runSandboxExec runs command in sandbox id, writing its stdout to w and
// stderr to errW, and returns an exitCodeError for a non-zero exit or timeout.
func runSandboxExec(c *ahclient.Client, w, errW io.Writer, id string, command []string, workdir string, timeout int) error {
	result, err := c.SandboxExec(id, command, workdir, timeout)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	fmt.Fprint(w, result.Stdout)
	fmt.Fprint(errW, result.Stderr)
	if result.Truncated {
		fmt.Fprintln(errW, "ahc: warning: command output was truncated by the server")
	}
	if result.TimedOut {
		fmt.Fprintln(errW, "ahc: command timed out")
		code := result.ExitCode
		if code == 0 {
			code = 124
		}
		return &exitCodeError{code: code, msg: fmt.Sprintf("command timed out (exit code %d)", code)}
	}
	if result.ExitCode != 0 {
		return &exitCodeError{code: result.ExitCode, msg: fmt.Sprintf("command exited with exit code %d", result.ExitCode)}
	}
	return nil
}

func newSandboxLogsCmd(root *rootState) *cobra.Command {
	var tail int
	var follow bool
	cmd := &cobra.Command{
		Use:     "logs <name-or-id>",
		Short:   "Show the sandbox's main process logs",
		Example: "  ahc sandbox logs web-copy --tail 200\n  ahc sandbox logs web-copy -f",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveSandboxID(c, args[0])
			if err != nil {
				return err
			}
			rc, err := c.GetSandboxLogs(id, follow, tail)
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(cmd.OutOrStdout(), rc)
			return err
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 100, "Number of lines from the end (max 10000)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream new log lines")
	return cmd
}

func newSandboxExtendCmd(root *rootState) *cobra.Command {
	var ttl string
	cmd := &cobra.Command{
		Use:     "extend <name-or-id> --ttl <duration>",
		Short:   "Set a sandbox to expire a TTL from now",
		Example: "  ahc sandbox extend scratch --ttl 2h",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			secs, err := parseTTL(ttl)
			if err != nil {
				return err
			}
			if secs == 0 {
				return fmt.Errorf("--ttl is required")
			}
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveSandboxID(c, args[0])
			if err != nil {
				return err
			}
			s, err := c.ExtendSandbox(id, secs)
			if err != nil {
				return fmt.Errorf("extend sandbox: %w", err)
			}
			if root.jsonOutput {
				return printJSON(cmd.OutOrStdout(), s)
			}
			printSuccess(cmd.OutOrStdout(), fmt.Sprintf("Sandbox %q now expires %s", s.Name, formatUnix(s.ExpiresAt)))
			return nil
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "", "New time to live from now (30m, 2h, or seconds)")
	return cmd
}

func newSandboxDeleteCmd(root *rootState) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete a sandbox now",
		Long: `Delete a sandbox: its container, workspace and record. Sandboxes are
disposable, so --confirm is accepted but not required.`,
		Example: "  ahc sandbox delete scratch",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveSandboxID(c, args[0])
			if err != nil {
				return err
			}
			if err := c.DeleteSandbox(id); err != nil {
				return fmt.Errorf("delete sandbox: %w", err)
			}
			if !root.jsonOutput {
				printSuccess(cmd.OutOrStdout(), fmt.Sprintf("Sandbox %q deleted", args[0]))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Accepted for symmetry with other delete commands; not required")
	return cmd
}

func newSandboxRunCmd(root *rootState) *cobra.Command {
	var o sandboxCreateOpts
	var workdir string
	var timeout int
	cmd := &cobra.Command{
		Use:   "run [source] [--ttl 10m] -- <command...>",
		Short: "Create a sandbox, run one command, delete it (exit code propagated)",
		Long: `Create a sandbox, run one command in it, then delete it, whatever the
command's result. If ahc is killed before it can delete the sandbox, the
server deletes it when its TTL (default 10m) expires.`,
		Example: `  ahc sandbox run --image python:3.12-slim -- python3 -c "print(2+2)"
  ahc sandbox run --template default --timeout 120 -- sh -c 'uname -a; id'`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("no command given: ahc sandbox run [source flags] -- <command...>")
			}
			req, err := buildSandboxRequest(o)
			if err != nil {
				return err
			}
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runSandboxRun(c, cmd.OutOrStdout(), cmd.ErrOrStderr(), req, args, workdir, timeout)
		},
	}
	addSandboxSourceFlags(cmd, &o, "10m")
	cmd.Flags().StringVar(&workdir, "workdir", "", "Working directory (default /workspace)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Command timeout in seconds (default 60, max 300)")
	return cmd
}

// runSandboxRun is create, exec, delete. The sandbox is always deleted; a
// delete failure is reported on errW and returned only when the command
// itself succeeded, so the command's exit code wins.
func runSandboxRun(c *ahclient.Client, w, errW io.Writer, req ahclient.CreateSandboxRequest, command []string, workdir string, timeout int) (err error) {
	s, err := c.CreateSandbox(req)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	defer func() {
		if derr := c.DeleteSandbox(s.ID); derr != nil && !isAPINotFound(derr) {
			fmt.Fprintf(errW, "ahc: warning: could not delete sandbox %s (the server deletes it at %s): %v\n", s.ID, formatUnix(s.ExpiresAt), derr)
			if err == nil {
				err = fmt.Errorf("delete sandbox: %w", derr)
			}
		}
	}()
	return runSandboxExec(c, w, errW, s.ID, command, workdir, timeout)
}

// isAPINotFound reports whether err is an API 404. For the final delete of
// "sandbox run" that means the sandbox is already gone (expired and reaped,
// or deleted by someone else), which is the outcome the delete wanted.
func isAPINotFound(err error) bool {
	var apiErr *ahclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// resolveSandboxID converts a name-or-ID argument into a sandbox ID: a
// 32-char hex ID is used as is, then an exact name, then a unique ID prefix.
func resolveSandboxID(c *ahclient.Client, arg string) (string, error) {
	if isHexID(arg) {
		return arg, nil
	}
	list, err := c.ListSandboxes("")
	if err != nil {
		return "", fmt.Errorf("list sandboxes: %w", err)
	}
	for _, s := range list {
		if s.Name == arg {
			return s.ID, nil // names are unique per tenant
		}
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	return resolveIDPrefix("sandbox", arg, ids)
}
