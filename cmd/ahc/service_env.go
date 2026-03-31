package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newServiceEnvCmd builds the `ahc service env` command group.
func newServiceEnvCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environment variables for a service",
		Long: `The env subcommand group manages encrypted environment variables for services.

Variables are stored encrypted at rest using the platform master key.
By default, values are masked when retrieved. Pass --reveal to see plaintext values.`,
	}

	cmd.AddCommand(
		newServiceEnvGetCmd(root),
		newServiceEnvSetCmd(root),
		newServiceEnvDeleteCmd(root),
	)

	return cmd
}

// ---- env get ----

func newServiceEnvGetCmd(root *rootState) *cobra.Command {
	var reveal bool

	cmd := &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "List environment variables for a service",
		Long: `List the environment variables set on a service.

By default, values are masked (shown as "***"). Pass --reveal to see the
plaintext values. Reveal operations are audit-logged on the server.`,
		Example: `  ahc service env get my-api
  ahc service env get my-api --reveal`,
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
			vars, err := c.GetServiceEnv(id, reveal)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, vars)
			}
			writeEnvTable(os.Stdout, vars, reveal)
			return nil
		},
	}

	cmd.Flags().BoolVar(&reveal, "reveal", false, "Show plaintext values (audit-logged)")
	return cmd
}

// writeEnvTable writes environment variables as a sorted key/value table.
func writeEnvTable(w interface{ Write([]byte) (int, error) }, vars map[string]string, reveal bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE")

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := vars[k]
		if !reveal && v != "" {
			v = "***"
		}
		fmt.Fprintf(tw, "%s\t%s\n", k, v)
	}
	tw.Flush()
}

// ---- env set ----

func newServiceEnvSetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "set <name-or-id> KEY=VALUE [KEY=VALUE...]",
		Short: "Set one or more environment variables",
		Long: `Set one or more environment variables on a service.

Variables are stored encrypted at rest. Each argument must be in KEY=VALUE
format. Values may contain '=' characters — only the first '=' is the delimiter.

Changes take effect on the next restart or redeploy. To apply immediately:
  ahc service redeploy <name-or-id>`,
		Example: `  ahc service env set my-api DATABASE_URL=postgres://host/db
  ahc service env set my-api FOO=bar BAR=baz
  ahc service env set my-api TOKEN=abc=def=ghi`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveServiceID(c, args[0])
			if err != nil {
				return err
			}
			vars, err := parseEnvPairs(args[1:])
			if err != nil {
				return err
			}
			if _, err := c.SetServiceEnv(id, vars); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "%s updated. Run 'ahc service redeploy %s' to apply.\n",
				pluralize(len(vars), "variable"), args[0])
			return nil
		},
	}
}

// ---- env delete ----

func newServiceEnvDeleteCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name-or-id> KEY",
		Short: "Delete an environment variable",
		Long: `Delete a single environment variable from a service.

The change takes effect on the next restart or redeploy. To apply immediately:
  ahc service redeploy <name-or-id>`,
		Example: `  ahc service env delete my-api OLD_SECRET`,
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
			if err := c.DeleteServiceEnv(id, args[1]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Variable %q deleted. Run 'ahc service redeploy %s' to apply.\n",
				args[1], args[0])
			return nil
		},
	}
}
