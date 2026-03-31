package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newEnvCmd builds the top-level `ahc env` command group.
func newEnvCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage instant environments (create, exec, stop, delete)",
		Long: `The env command group manages instant sandboxed environments for AI agents
and developers. Each environment is an isolated container with a configurable
lease time, language template, and full exec access.

Environments are ideal for:
  - Running AI agent workloads in isolation
  - Short-lived development sandboxes
  - Testing code without affecting production services

Quick start:
  # Create a Go dev environment
  ahc env create my-env --template tmpl-golang

  # Run a command inside it
  ahc env exec my-env -- go version

  # List available templates
  ahc env templates

  # Stop and delete when done
  ahc env stop my-env
  ahc env delete my-env --confirm

Full command reference:
  ahc env list                              List all environments
  ahc env create <name>                     Create a new environment
  ahc env get <name-or-id>                  Get environment details
  ahc env exec <name-or-id> -- <cmd...>     Execute a command
  ahc env stop <name-or-id>                 Stop a running environment
  ahc env start <name-or-id>                Start a stopped environment
  ahc env delete <name-or-id> --confirm     Delete an environment permanently
  ahc env templates                         List available templates`,
	}

	cmd.AddCommand(
		newEnvListCmd(root),
		newEnvCreateCmd(root),
		newEnvGetCmd(root),
		newEnvExecCmd(root),
		newEnvStopCmd(root),
		newEnvStartCmd(root),
		newEnvDeleteCmd(root),
		newEnvTemplatesCmd(root),
	)

	return cmd
}

// newEnvListCmd implements `ahc env list`.
func newEnvListCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all environments",
		Long: `List all instant environments for your tenant.

Displays a table with NAME, STATUS, TEMPLATE, ID (truncated), and EXPIRES time.`,
		Example: `  ahc env list
  ahc env list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				envs, err := c.ListEnvironments()
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), envs)
			}
			return runEnvList(c, cmd.OutOrStdout())
		},
	}
}

// runEnvList is the testable core of the env list command.
func runEnvList(c *ahclient.Client, w io.Writer) error {
	envs, err := c.ListEnvironments()
	if err != nil {
		return err
	}

	if len(envs) == 0 {
		fmt.Fprintln(w, "No environments found.")
		return nil
	}

	headers := []string{"NAME", "STATUS", "TEMPLATE", "ID", "EXPIRES"}
	rows := make([][]string, 0, len(envs))
	for _, e := range envs {
		shortID := e.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		expires := "-"
		if e.LeaseExpiresAt != nil {
			expires = time.Unix(*e.LeaseExpiresAt, 0).Format("2006-01-02 15:04")
		}
		rows = append(rows, []string{
			e.Name,
			e.Status,
			e.TemplateID,
			shortID,
			expires,
		})
	}
	printTable(w, headers, rows)
	return nil
}

// newEnvCreateCmd implements `ahc env create <name> [--template <tmpl_id>] [--lease <seconds>]`.
func newEnvCreateCmd(root *rootState) *cobra.Command {
	var (
		templateFlag string
		leaseFlag    int
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new instant environment",
		Long: `Create a new sandboxed instant environment.

Use --template to select a pre-configured language environment (see 'ahc env templates').
Use --lease to set how long (in seconds) the environment stays active before auto-stopping
(default: server default, typically 3600 seconds / 1 hour).

Examples:
  ahc env create my-env
  ahc env create my-env --template tmpl-golang
  ahc env create my-env --template tmpl-node --lease 7200`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			var leasePtr *int
			if cmd.Flags().Changed("lease") {
				leasePtr = &leaseFlag
			}
			return runEnvCreate(c, cmd.OutOrStdout(), args[0], templateFlag, leasePtr)
		},
	}

	cmd.Flags().StringVar(&templateFlag, "template", "", "Template ID to use (see 'ahc env templates')")
	cmd.Flags().IntVar(&leaseFlag, "lease", 3600, "Lease duration in seconds (default: 3600)")
	return cmd
}

// runEnvCreate is the testable core of the env create command.
func runEnvCreate(c *ahclient.Client, w io.Writer, name, templateID string, leaseDuration *int) error {
	fmt.Fprintf(w, "Creating environment %q...\n", name)

	req := ahclient.CreateEnvironmentRequest{
		Name:                 name,
		TemplateID:           templateID,
		LeaseDurationSeconds: leaseDuration,
	}

	env, err := c.CreateEnvironment(req)
	if err != nil {
		return fmt.Errorf("create environment: %w", err)
	}

	printSuccess(w, fmt.Sprintf("Environment %q created (id: %s, status: %s)", env.Name, env.ID[:8], env.Status))
	fmt.Fprintf(w, "\nRun a command:\n  ahc env exec %s -- <command>\n", env.Name)
	return nil
}

// newEnvGetCmd implements `ahc env get <name-or-id>`.
func newEnvGetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "Get details about an environment",
		Long: `Display detailed information about a single environment.

Accepts either an environment name or a 32-character hex ID.`,
		Example: `  ahc env get my-env
  ahc env get aabbccdd11223344aabbccdd11223344
  ahc env get my-env --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				id, err := resolveEnvID(c, args[0])
				if err != nil {
					return err
				}
				env, err := c.GetEnvironment(id)
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), env)
			}
			return runEnvGet(c, cmd.OutOrStdout(), args[0])
		},
	}
}

// runEnvGet is the testable core of the env get command.
func runEnvGet(c *ahclient.Client, w io.Writer, nameOrID string) error {
	id, err := resolveEnvID(c, nameOrID)
	if err != nil {
		return err
	}
	env, err := c.GetEnvironment(id)
	if err != nil {
		return err
	}
	printEnvDetails(w, env)
	return nil
}

// printEnvDetails writes a formatted detail view of an environment.
func printEnvDetails(w io.Writer, env *ahclient.Environment) {
	fmt.Fprintf(w, "ID:         %s\n", env.ID)
	fmt.Fprintf(w, "Name:       %s\n", env.Name)
	fmt.Fprintf(w, "Status:     %s\n", env.Status)
	fmt.Fprintf(w, "Template:   %s\n", env.TemplateID)
	if env.LeaseExpiresAt != nil {
		fmt.Fprintf(w, "Expires:    %s\n", time.Unix(*env.LeaseExpiresAt, 0).Format(time.RFC3339))
	}
	fmt.Fprintf(w, "Created:    %s\n", formatTimestamp(env.CreatedAt))
}

// newEnvExecCmd implements `ahc env exec <name-or-id> -- <command...>`.
func newEnvExecCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec <name-or-id> -- <command...>",
		Short: "Execute a command in an environment",
		Long: `Execute a command inside a running environment and print its output.

The -- separator is required to separate ahc flags from the command to run.
The command runs synchronously; ahc waits for it to complete.

Exit codes from the executed command are propagated — if the command exits
with code 1, 'ahc env exec' also exits with code 1.

Examples:
  ahc env exec my-env -- go version
  ahc env exec my-env -- bash -c "echo hello && ls"
  ahc env exec my-env -- python3 script.py`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runEnvExec(c, os.Stdout, args[0], args[1:])
		},
	}
	return cmd
}

// runEnvExec is the testable core of the env exec command.
func runEnvExec(c *ahclient.Client, w io.Writer, nameOrID string, command []string) error {
	id, err := resolveEnvID(c, nameOrID)
	if err != nil {
		return err
	}

	result, err := c.EnvironmentExec(id, command, "", 0)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	if result.Stdout != "" {
		fmt.Fprint(w, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(w, result.Stderr)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("command exited with exit code %d", result.ExitCode)
	}
	return nil
}

// newEnvStopCmd implements `ahc env stop <name-or-id>`.
func newEnvStopCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name-or-id>",
		Short: "Stop a running environment",
		Long: `Stop a running environment. The environment's state is preserved and
it can be restarted with 'ahc env start'.

Example:
  ahc env stop my-env`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runEnvStop(c, cmd.OutOrStdout(), args[0])
		},
	}
}

// runEnvStop is the testable core of the env stop command.
func runEnvStop(c *ahclient.Client, w io.Writer, nameOrID string) error {
	id, err := resolveEnvID(c, nameOrID)
	if err != nil {
		return err
	}
	_, err = c.StopEnvironment(id)
	if err != nil {
		return fmt.Errorf("stop environment: %w", err)
	}
	printSuccess(w, fmt.Sprintf("Environment %q stopped", nameOrID))
	return nil
}

// newEnvStartCmd implements `ahc env start <name-or-id>`.
func newEnvStartCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "start <name-or-id>",
		Short: "Start a stopped environment",
		Long: `Start a previously stopped environment.

Example:
  ahc env start my-env`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runEnvStart(c, cmd.OutOrStdout(), args[0])
		},
	}
}

// runEnvStart is the testable core of the env start command.
func runEnvStart(c *ahclient.Client, w io.Writer, nameOrID string) error {
	id, err := resolveEnvID(c, nameOrID)
	if err != nil {
		return err
	}
	_, err = c.StartEnvironment(id)
	if err != nil {
		return fmt.Errorf("start environment: %w", err)
	}
	printSuccess(w, fmt.Sprintf("Environment %q started", nameOrID))
	return nil
}

// newEnvDeleteCmd implements `ahc env delete <name-or-id> --confirm`.
func newEnvDeleteCmd(root *rootState) *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete an environment permanently",
		Long: `Permanently delete an environment and all its data.

This action is irreversible. You must pass --confirm to proceed.
The container, filesystem, and associated resources are removed.

Example:
  ahc env delete my-env --confirm`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runEnvDelete(c, args[0], confirm)
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false, "Required: confirm you want to permanently delete the environment")
	return cmd
}

// runEnvDelete is the testable core of the env delete command.
func runEnvDelete(c *ahclient.Client, nameOrID string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("refusing to delete without --confirm; re-run with --confirm to proceed")
	}
	id, err := resolveEnvID(c, nameOrID)
	if err != nil {
		return err
	}
	return c.DeleteEnvironment(id)
}

// newEnvTemplatesCmd implements `ahc env templates`.
func newEnvTemplatesCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "templates",
		Short: "List available environment templates",
		Long: `List all available environment templates.

Templates define the base image, resource limits, and configuration for
new environments. Pass a template ID to 'ahc env create --template <id>'.

Example:
  ahc env templates
  ahc env templates --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				templates, err := c.ListEnvironmentTemplates()
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), templates)
			}
			return runEnvTemplates(c, cmd.OutOrStdout())
		},
	}
}

// runEnvTemplates is the testable core of the env templates command.
func runEnvTemplates(c *ahclient.Client, w io.Writer) error {
	templates, err := c.ListEnvironmentTemplates()
	if err != nil {
		return err
	}

	if len(templates) == 0 {
		fmt.Fprintln(w, "No templates available.")
		return nil
	}

	headers := []string{"ID", "NAME", "BASE_IMAGE", "MEMORY_MB", "CPU_MILLICORES", "DESCRIPTION"}
	rows := make([][]string, 0, len(templates))
	for _, t := range templates {
		desc := t.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}
		rows = append(rows, []string{
			t.ID,
			t.Name,
			t.BaseImage,
			fmt.Sprintf("%d", t.MemoryMB),
			fmt.Sprintf("%d", t.CPUMillis),
			desc,
		})
	}
	printTable(w, headers, rows)
	return nil
}

// resolveEnvID converts a name-or-ID argument into an environment ID.
// If arg is exactly 32 lowercase hex characters, it is used directly.
// Otherwise, ListEnvironments is called and filtered by name.
func resolveEnvID(c *ahclient.Client, arg string) (string, error) {
	if isHexID(arg) {
		return arg, nil
	}

	envs, err := c.ListEnvironments()
	if err != nil {
		return "", fmt.Errorf("list environments: %w", err)
	}

	var matches []ahclient.Environment
	for _, e := range envs {
		if e.Name == arg {
			matches = append(matches, e)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("environment %q not found", arg)
	case 1:
		return matches[0].ID, nil
	default:
		ids := make([]string, len(matches))
		for i, e := range matches {
			ids[i] = e.ID[:8]
		}
		return "", fmt.Errorf("ambiguous: %d environments named %q (ids: %s)", len(matches), arg, strings.Join(ids, ", "))
	}
}
