package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newDbCmd builds the top-level `ahc db` command group.
func newDbCmd(root *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage databases (provision, list, get, delete)",
		Long: `The db command group lets you provision, inspect, and delete databases
attached to your services on the agentic-hosting platform.

Supported database types: postgres, redis

Use 'ahc db provision' to create a database, link it to a service, and
automatically set the correct connection-string environment variable.`,
	}

	cmd.AddCommand(
		newDbListCmd(root),
		newDbGetCmd(root),
		newDbProvisionCmd(root),
		newDbDeleteCmd(root),
	)

	return cmd
}

// ---- list ----

func newDbListCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all databases",
		Long: `List all databases provisioned for your tenant.

Displays a table with NAME, TYPE, STATUS, SERVICE association, and a
truncated ID.`,
		Example: `  ahc db list
  ahc db list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			dbs, err := c.ListDatabases()
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, dbs)
			}
			writeDbTable(os.Stdout, dbs)
			return nil
		},
	}
}

// writeDbTable writes databases to w as a formatted table.
func writeDbTable(w io.Writer, dbs []ahclient.Database) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tSTATUS\tSERVICE\tID")
	for _, db := range dbs {
		shortID := db.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		// The Database type does not expose a service name/ID directly;
		// show "-" as placeholder until the API returns that field.
		service := "-"
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			db.Name, db.Type, db.Status, service, shortID)
	}
	tw.Flush()
}

// ---- get ----

func newDbGetCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name-or-id>",
		Short: "Get details about a database",
		Long: `Display detailed information about a single database.

Accepts either a database name or a 32-character hex database ID.
When a name is given, all databases are fetched and filtered by name.

The connection string host:port is shown but the password is masked.`,
		Example: `  ahc db get my-postgres
  ahc db get aabbccdd11223344aabbccdd11223344
  ahc db get my-postgres --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			id, err := resolveDbID(c, args[0])
			if err != nil {
				return err
			}
			db, err := c.GetDatabase(id)
			if err != nil {
				return err
			}
			if root.jsonOutput {
				return writeJSON(os.Stdout, db)
			}
			printDatabase(os.Stdout, db)
			return nil
		},
	}
}

func printDatabase(w io.Writer, db *ahclient.Database) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ID:\t%s\n", db.ID)
	fmt.Fprintf(tw, "Name:\t%s\n", db.Name)
	fmt.Fprintf(tw, "Type:\t%s\n", db.Type)
	fmt.Fprintf(tw, "Status:\t%s\n", db.Status)
	if db.Host != "" {
		fmt.Fprintf(tw, "Host:\t%s\n", db.Host)
	}
	if db.Port != 0 {
		fmt.Fprintf(tw, "Port:\t%d\n", db.Port)
	}
	if db.DBName != "" {
		fmt.Fprintf(tw, "DB Name:\t%s\n", db.DBName)
	}
	if db.Username != "" {
		fmt.Fprintf(tw, "Username:\t%s\n", db.Username)
	}
	if db.ConnectionString != "" {
		fmt.Fprintf(tw, "Connection:\t%s\n", maskConnString(db.ConnectionString))
	}
	fmt.Fprintf(tw, "Created:\t%s\n", time.Unix(db.CreatedAt, 0).Format(time.RFC3339))
	fmt.Fprintf(tw, "Updated:\t%s\n", time.Unix(db.UpdatedAt, 0).Format(time.RFC3339))
	tw.Flush()
}

// ---- provision ----

func newDbProvisionCmd(root *rootState) *cobra.Command {
	var dbName string

	cmd := &cobra.Command{
		Use:   "provision <service-name> <postgres|redis>",
		Short: "Provision a database and link it to a service",
		Long: `Provision a new database, wait for it to become available, retrieve
its connection string, and set the appropriate environment variable on
the given service. The service is then restarted so the new variable
takes effect.

For postgres, DATABASE_URL is set on the service.
For redis,    REDIS_URL    is set on the service.

The --name flag controls the database name (defaults to
"<service-name>-<type>").`,
		Example: `  ahc db provision my-api postgres
  ahc db provision my-api redis --name cache
  ahc db provision my-api postgres --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			serviceName := args[0]
			dbType := args[1]

			if dbType != "postgres" && dbType != "redis" {
				return fmt.Errorf("unsupported database type %q: must be 'postgres' or 'redis'", dbType)
			}

			c, err := getClient(root)
			if err != nil {
				return err
			}

			// Resolve service ID
			serviceID, err := resolveServiceID(c, serviceName)
			if err != nil {
				return fmt.Errorf("service %q: %w", serviceName, err)
			}

			// Determine database name
			name := dbName
			if name == "" {
				name = serviceName + "-" + dbType
			}

			// Create the database
			fmt.Fprintf(os.Stdout, "Provisioning %s database %q...\n", dbType, name)
			db, err := c.CreateDatabase(ahclient.CreateDatabaseRequest{
				Name: name,
				Type: dbType,
			})
			if err != nil {
				return fmt.Errorf("create database: %w", err)
			}

			if root.jsonOutput {
				return writeJSON(os.Stdout, db)
			}

			fmt.Fprintf(os.Stdout, "Database %q created (id: %s)\n", db.Name, db.ID)

			// Poll until running
			fmt.Fprint(os.Stdout, "Waiting for database to become available")
			db, err = pollDbUntilRunning(c, db.ID, os.Stdout)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, " done.")

			// Get connection string
			connStr, err := c.GetDatabaseConnectionString(db.ID)
			if err != nil {
				return fmt.Errorf("get connection string: %w", err)
			}

			// Set env var on service
			envKey := envKeyForType(dbType)
			fmt.Fprintf(os.Stdout, "Setting %s on service %q...\n", envKey, serviceName)
			envVars := map[string]string{envKey: connStr}
			if _, err := c.SetServiceEnv(serviceID, envVars); err != nil {
				return fmt.Errorf("set env var %s: %w", envKey, err)
			}

			// Restart service
			fmt.Fprintf(os.Stdout, "Restarting service %q...\n", serviceName)
			if _, err := c.RestartService(serviceID); err != nil {
				return fmt.Errorf("restart service: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Done. %s is ready. Connection host: %s\n",
				db.Name, maskConnString(connStr))
			return nil
		},
	}

	cmd.Flags().StringVar(&dbName, "name", "", "Database name (defaults to <service>-<type>)")
	return cmd
}

// pollDbUntilRunning polls GetDatabase until status is "running" or an error occurs.
// It prints a dot per iteration to indicate progress.
func pollDbUntilRunning(c *ahclient.Client, dbID string, progress io.Writer) (*ahclient.Database, error) {
	for i := 0; i < 60; i++ {
		db, err := c.GetDatabase(dbID)
		if err != nil {
			return nil, fmt.Errorf("poll database: %w", err)
		}
		switch db.Status {
		case "running":
			return db, nil
		case "error", "failed":
			return nil, fmt.Errorf("database provisioning failed with status %q", db.Status)
		}
		fmt.Fprint(progress, ".")
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for database to become available")
}

// ---- delete ----

func newDbDeleteCmd(root *rootState) *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "delete <name-or-id>",
		Short: "Delete a database",
		Long: `Permanently delete a database and all its data.

This action is irreversible. You must pass --confirm to proceed.
The database container and its storage volume are removed.

WARNING: Any service connected to this database will lose its connection.
Update the service's DATABASE_URL or REDIS_URL before deleting.`,
		Example: `  ahc db delete my-postgres --confirm`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			return runDbDelete(c, args[0], confirm)
		},
	}

	cmd.Flags().BoolVar(&confirm, "confirm", false, "Required: confirm you want to permanently delete the database")
	return cmd
}

// runDbDelete is the testable core of the delete command.
func runDbDelete(c *ahclient.Client, nameOrID string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("refusing to delete without --confirm; re-run with --confirm to proceed")
	}
	id, err := resolveDbID(c, nameOrID)
	if err != nil {
		return err
	}
	return c.DeleteDatabase(id)
}

// ---- helpers ----

// resolveDbID converts a name-or-ID argument into a database ID.
// If arg is exactly 32 lowercase hex characters, it is used directly.
// Otherwise, ListDatabases is called and the result is filtered by name.
func resolveDbID(c *ahclient.Client, arg string) (string, error) {
	if isHexID(arg) {
		return arg, nil
	}

	dbs, err := c.ListDatabases()
	if err != nil {
		return "", fmt.Errorf("list databases: %w", err)
	}

	var matches []ahclient.Database
	for _, db := range dbs {
		if db.Name == arg {
			matches = append(matches, db)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("database %q not found", arg)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("ambiguous: %d databases named %q", len(matches), arg)
	}
}

// maskConnString returns only the host:port portion of a connection URL string.
// The password is replaced with "***". For non-URL strings, the input is returned as-is.
func maskConnString(s string) string {
	if s == "" {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		// Not a parseable URL — return as-is
		return s
	}
	// Rebuild showing scheme://host only
	masked := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
	}
	return masked.String()
}

// envKeyForType returns the environment variable key for the given database type.
// "postgres" → "DATABASE_URL", "redis" → "REDIS_URL".
// Unknown types return "<TYPE>_URL" so they are always distinct from known keys.
func envKeyForType(dbType string) string {
	switch dbType {
	case "postgres":
		return "DATABASE_URL"
	case "redis":
		return "REDIS_URL"
	default:
		// Return a type-specific key so callers can detect unknowns.
		// Using uppercase dbType keeps it env-var–safe.
		return strings.ToUpper(dbType) + "_URL"
	}
}
