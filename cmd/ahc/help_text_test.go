package main

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func helpText(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--help"))
	require.NoError(t, cmd.Execute())
	return out.String()
}

// Regression: the env help examples used template IDs that do not exist
// (tmpl-golang, tmpl-node). Every --template value in the help must be a
// seeded template ID or name, which is how the server looks templates up.
func TestEnvHelp_TemplateExamplesExist(t *testing.T) {
	db := testutil.NewStateDB(t)
	known := map[string]bool{}
	rows, err := db.Query(`SELECT id, name FROM environment_templates`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, name string
		require.NoError(t, rows.Scan(&id, &name))
		known[id], known[name] = true, true
	}

	re := regexp.MustCompile(`ahc env create \S+ --template (\S+)`)
	found := 0
	for _, args := range [][]string{{"env"}, {"env", "create"}} {
		for _, m := range re.FindAllStringSubmatch(helpText(t, args...), -1) {
			found++
			assert.True(t, known[m[1]], "help for %v uses unknown template %q", args, m[1])
		}
	}
	assert.Positive(t, found)
}

// Regression: the sandbox help said sandboxes never have network egress,
// though --egress internet exists.
func TestSandboxHelp_MentionsEgressInternet(t *testing.T) {
	out := helpText(t, "sandbox")
	assert.NotContains(t, out, "A sandbox has no network egress (")
	assert.Contains(t, out, "With --egress internet")
}
