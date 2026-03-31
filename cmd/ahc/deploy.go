package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/spf13/cobra"
)

// isGitURL returns true if source looks like a git URL (starts with http:// or https://).
func isGitURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// pollUntilRunning polls GetService every 2s until the service reaches "running",
// an error/stopped state, or maxAttempts is exhausted. Prints a dot per iteration.
// maxAttempts of 0 means use the default (150 = 5 minutes at 2s intervals).
func pollUntilRunning(c *ahclient.Client, w io.Writer, serviceID string, maxAttempts int) error {
	if maxAttempts <= 0 {
		maxAttempts = 150 // 5 minutes at 2s intervals
	}
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		svc, err := c.GetService(serviceID)
		if err != nil {
			return fmt.Errorf("polling service status: %w", err)
		}
		switch svc.Status {
		case "running":
			fmt.Fprintln(w) // end the dots line
			if svc.URL != "" {
				fmt.Fprintf(w, "  URL: %s\n", svc.URL)
			}
			return nil
		case "error", "crashed":
			fmt.Fprintln(w)
			if svc.LastError != "" {
				return fmt.Errorf("service entered error state: %s", svc.LastError)
			}
			return fmt.Errorf("service entered error state (status: %s) — check logs with: ahc build logs %s", svc.Status, svc.Name)
		default:
			fmt.Fprint(w, ".")
		}
	}
	fmt.Fprintln(w)
	return fmt.Errorf("timeout: service did not reach running state within %d attempts — check logs with: ahc build logs %s", maxAttempts, serviceID)
}

// runDeployImage creates a service with the given Docker image and polls until running.
func runDeployImage(c *ahclient.Client, w io.Writer, image, name string, port int) error {
	fmt.Fprintf(w, "Deploying image %s as %q...\n", image, name)

	svc, err := c.CreateService(ahclient.CreateServiceRequest{
		Name:  name,
		Image: image,
		Port:  port,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}

	fmt.Fprintf(w, "Service %q created (id: %s)\n", svc.Name, svc.ID[:8])
	fmt.Fprint(w, "Waiting for service to start")

	return pollUntilRunning(c, w, svc.ID, 0)
}

// streamBuildLogs reads from the build log stream and prints each line with a "[build]" prefix.
func streamBuildLogs(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fmt.Fprintf(w, "[build] %s\n", scanner.Text())
	}
}

// runDeployGit creates a service (or finds existing by name), starts a build from the
// given git URL, streams build logs, then polls until the service is running.
func runDeployGit(c *ahclient.Client, w io.Writer, gitURL, name string, port int) error {
	fmt.Fprintf(w, "Deploying %s as %q...\n", gitURL, name)

	// Create the service (or use existing if name matches).
	svc, err := c.CreateService(ahclient.CreateServiceRequest{
		Name:  name,
		Image: "pending", // placeholder — will be replaced by the build
		Port:  port,
	})
	if err != nil {
		// If create failed, try to find existing service by name.
		services, listErr := c.ListServices()
		if listErr != nil {
			return fmt.Errorf("create service: %w", err)
		}
		var found *ahclient.Service
		for i := range services {
			if services[i].Name == name {
				found = &services[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("create service: %w", err)
		}
		svc = found
		fmt.Fprintf(w, "Using existing service %q (id: %s)\n", svc.Name, svc.ID[:8])
	} else {
		fmt.Fprintf(w, "Service %q created (id: %s)\n", svc.Name, svc.ID[:8])
	}

	// Start a build.
	fmt.Fprintln(w, "Starting build...")
	buildResp, err := c.StartBuild(svc.ID, ahclient.StartBuildRequest{
		SourceType: "git",
		SourceURL:  gitURL,
	})
	if err != nil {
		return fmt.Errorf("start build: %w", err)
	}

	fmt.Fprintf(w, "Build started (id: %s)\n", buildResp.BuildID)

	// Stream build logs.
	logsReader, err := c.GetBuildLogs(svc.ID, buildResp.BuildID, true)
	if err != nil {
		return fmt.Errorf("get build logs: %w", err)
	}
	defer logsReader.Close()
	streamBuildLogs(logsReader, w)

	// Poll until service is running.
	fmt.Fprint(w, "Waiting for service to start")
	return pollUntilRunning(c, w, svc.ID, 0)
}

// runRedeploy finds a service by name and calls RedeployService, then prints the URL.
func runRedeploy(c *ahclient.Client, w io.Writer, name string) error {
	serviceID, err := resolveServiceID(c, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "Redeploying service %q...\n", name)

	svc, err := c.RedeployService(serviceID)
	if err != nil {
		return fmt.Errorf("redeploy service: %w", err)
	}

	printSuccess(w, fmt.Sprintf("Redeployment triggered for %q", svc.Name))
	if svc.URL != "" {
		fmt.Fprintf(w, "  URL: %s\n", svc.URL)
	}
	return nil
}

// newDeployCmd builds the `ahc deploy` cobra command.
func newDeployCmd(root *rootState) *cobra.Command {
	var (
		portFlag     int
		redeployFlag bool
	)

	cmd := &cobra.Command{
		Use:   "deploy [<source> <service-name>] [--redeploy <service-name>]",
		Short: "Deploy a service from a Docker image or git repository",
		Long: `Deploy a service from a Docker image or git repository.

Examples:
  ahc deploy nginx:alpine my-site --port 80
  ahc deploy https://github.com/org/repo my-app --port 3000
  ahc deploy --redeploy my-app

The deploy command detects the source type automatically:
  - Docker images: pulled and started directly
  - Git URLs: cloned, built with Nixpacks, then deployed`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --redeploy mode: ahc deploy --redeploy <service-name>
			if redeployFlag {
				if len(args) == 0 {
					return fmt.Errorf("--redeploy requires a service name: ahc deploy --redeploy <service-name>")
				}
				c, err := getClient(root)
				if err != nil {
					return err
				}
				return runRedeploy(c, cmd.OutOrStdout(), args[0])
			}

			// Normal deploy mode: ahc deploy <source> <service-name> [--port N]
			if len(args) < 2 {
				return fmt.Errorf("usage: ahc deploy <source> <service-name> [--port N]\n  source is a Docker image (e.g. nginx:alpine) or git URL (e.g. https://github.com/org/repo)")
			}

			source := args[0]
			name := args[1]

			c, err := getClient(root)
			if err != nil {
				return err
			}

			if isGitURL(source) {
				return runDeployGit(c, cmd.OutOrStdout(), source, name, portFlag)
			}
			return runDeployImage(c, cmd.OutOrStdout(), source, name, portFlag)
		},
	}

	cmd.Flags().IntVar(&portFlag, "port", 0, "Port the service listens on")
	cmd.Flags().BoolVar(&redeployFlag, "redeploy", false, "Redeploy an existing service by name")

	return cmd
}
