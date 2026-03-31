package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Version, Commit, and Date are set at build time via ldflags:
//
//	go build -ldflags "-X main.Version=0.1.0 -X main.Commit=abc1234 -X main.Date=2026-03-31" ./cmd/ahc
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

func versionString() string {
	v := Version
	if v == "" {
		v = "dev"
	}
	c := Commit
	if c == "" {
		c = "unknown"
	}
	d := Date
	if d == "" {
		d = "unknown"
	}
	return fmt.Sprintf("ahc version %s (commit %s, built %s)", v, c, d)
}

func newVersionCmd(root *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  `Print the ahc version, git commit, and build date.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root.jsonOutput {
				v := Version
				if v == "" {
					v = "dev"
				}
				c := Commit
				if c == "" {
					c = "unknown"
				}
				d := Date
				if d == "" {
					d = "unknown"
				}
				return printJSON(cmd.OutOrStdout(), map[string]string{
					"version": v,
					"commit":  c,
					"date":    d,
				})
			}

			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return err
		},
	}
}

// versionJSON is used internally in tests to parse version output when --json is set.
type versionJSON struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// parseVersionJSON parses JSON version output. Used in tests.
func parseVersionJSON(data []byte) (*versionJSON, error) {
	var v versionJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
