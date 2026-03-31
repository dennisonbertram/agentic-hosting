package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// newStatusCmd builds the `ahc status` command.
func newStatusCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show system health and resource status",
		Long: `Show system health and resource status.

Examples:
  ahc status
  ahc status --json

Exit codes:
  0  All systems healthy
  1  One or more services unhealthy or disk critically full`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient(root)
			if err != nil {
				return err
			}
			code, err := runStatusCmd(c, os.Stdout, root.jsonOutput)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
}

// statusReport is the combined JSON payload returned by --json.
type statusReport struct {
	Health    *ahclient.DetailedHealthResponse `json:"health"`
	Services  []ahclient.Service               `json:"services"`
	Databases []ahclient.Database              `json:"databases"`
}

// isUnhealthyServiceStatus returns true for service statuses that indicate failure.
func isUnhealthyServiceStatus(status string) bool {
	return status == "circuit_open" || status == "failed"
}

// runStatusCmd is the testable core of the status command.
// It fetches health, services, and databases, renders the appropriate output,
// and returns an exit code (0 = healthy, 1 = unhealthy).
func runStatusCmd(c *ahclient.Client, w io.Writer, jsonOutput bool) (int, error) {
	// Fetch all three data sources concurrently would be ideal but for simplicity:
	health, err := c.HealthDetailed(false)
	if err != nil {
		return 0, fmt.Errorf("health: %w", err)
	}
	services, err := c.ListServices()
	if err != nil {
		return 0, fmt.Errorf("services: %w", err)
	}
	databases, err := c.ListDatabases()
	if err != nil {
		return 0, fmt.Errorf("databases: %w", err)
	}

	// Determine exit code
	exitCode := 0
	diskCritical := health.Disk.UsedPercent > 90.0
	if diskCritical {
		exitCode = 1
	}
	for _, svc := range services {
		if isUnhealthyServiceStatus(svc.Status) {
			exitCode = 1
			break
		}
	}

	if jsonOutput {
		report := statusReport{
			Health:    health,
			Services:  services,
			Databases: databases,
		}
		if err := printJSON(w, report); err != nil {
			return 0, err
		}
		return exitCode, nil
	}

	// ---- System health block ----
	fmt.Fprintln(w, "System Health")
	fmt.Fprintln(w, "-------------")

	// Status
	statusLabel := health.Status
	if health.Status == "ok" {
		fmt.Fprintf(w, "  Status:  %s\n", colored(colorGreen, statusLabel))
	} else {
		fmt.Fprintf(w, "  Status:  %s\n", colored(colorRed, statusLabel))
	}

	// Disk usage with color
	diskPct := health.Disk.UsedPercent
	diskStr := fmt.Sprintf("%.1f%%", diskPct)
	switch {
	case diskPct > 90:
		fmt.Fprintf(w, "  Disk:    %s (critical)\n", colored(colorRed, diskStr))
	case diskPct > 80:
		fmt.Fprintf(w, "  Disk:    %s\n", colored(colorYellow, "! "+diskStr+" warn"))
	default:
		fmt.Fprintf(w, "  Disk:    %s\n", colored(colorGreen, diskStr))
	}

	// Docker
	if health.Docker.Available {
		fmt.Fprintf(w, "  Docker:  %s\n", health.Docker.Version)
	} else {
		fmt.Fprintf(w, "  Docker:  %s\n", colored(colorRed, "unavailable"))
	}

	// gVisor
	if health.GVisor.Available {
		fmt.Fprintf(w, "  gVisor:  %s\n", colored(colorGreen, "available"))
	} else {
		fmt.Fprintf(w, "  gVisor:  %s\n", colored(colorYellow, "unavailable"))
	}
	fmt.Fprintln(w)

	// ---- Services table ----
	fmt.Fprintln(w, "Services")
	if len(services) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		headers := []string{"NAME", "STATUS", "RESTARTS"}
		rows := make([][]string, 0, len(services))
		for _, svc := range services {
			status := svc.Status
			// Color unhealthy statuses red
			if isUnhealthyServiceStatus(svc.Status) {
				status = colored(colorRed, svc.Status)
			}
			rows = append(rows, []string{
				svc.Name,
				status,
				strconv.Itoa(svc.CrashCount),
			})
		}
		printTable(w, headers, rows)
	}
	fmt.Fprintln(w)

	// ---- Databases table ----
	fmt.Fprintln(w, "Databases")
	if len(databases) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		headers := []string{"NAME", "TYPE", "STATUS"}
		rows := make([][]string, 0, len(databases))
		for _, db := range databases {
			rows = append(rows, []string{db.Name, db.Type, db.Status})
		}
		printTable(w, headers, rows)
	}

	return exitCode, nil
}
