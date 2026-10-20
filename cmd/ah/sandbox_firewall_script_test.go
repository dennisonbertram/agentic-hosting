package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const firewallScript = "../../docs/ops/sandbox-egress-firewall.sh"

func runFirewallScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{firewallScript}, args...)...)
	cmd.Env = append(os.Environ(), "AH_SANDBOX_EGRESS_NETWORK=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// freshHost is the ruleset of a host whose INPUT already accepts SSH and
// whose Docker (iptables backend) hooks DOCKER-USER, which ends with RETURN.
func freshHost() map[string][]string {
	return map[string][]string{
		"iptables FORWARD":     {"-A FORWARD -j DOCKER-USER", "-A FORWARD -j DOCKER-FORWARD"},
		"iptables INPUT":       {"-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT"},
		"iptables DOCKER-USER": {"-A DOCKER-USER -j RETURN"},
		"ip6tables INPUT":      {},
		"ip6tables FORWARD":    {},
	}
}

// wouldRun returns the script's "would run" lines.
func wouldRun(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "would run: ") {
			lines = append(lines, strings.TrimPrefix(line, "would run: "))
		}
	}
	return lines
}

// simulate applies "would run" lines to chains (in place): -A, -I 1, -F and
// -D (every copy, as delete_tagged does). Other commands are ignored.
func simulate(t *testing.T, chains map[string][]string, lines []string) {
	t.Helper()
	for _, line := range lines {
		f := strings.Fields(line)
		for i, w := range f { // drop "(repeated while present)" and similar notes
			if strings.HasPrefix(w, "(") {
				f = f[:i]
				break
			}
		}
		if len(f) < 4 || (f[0] != "iptables" && f[0] != "ip6tables") || f[1] != "-w" {
			continue
		}
		key := f[0] + " " + f[3]
		switch f[2] {
		case "-A":
			chains[key] = append(chains[key], "-A "+strings.Join(f[3:], " "))
		case "-I":
			require.Equal(t, "1", f[4], "rules are inserted at position 1: %s", line)
			rule := "-A " + f[3] + " " + strings.Join(f[5:], " ")
			chains[key] = append([]string{rule}, chains[key]...)
		case "-F":
			chains[key] = nil
		case "-D":
			rule := "-A " + strings.Join(f[3:], " ")
			kept := chains[key][:0:0]
			for _, r := range chains[key] {
				if r != rule {
					kept = append(kept, r)
				}
			}
			chains[key] = kept
		}
	}
}

func toRules(chains map[string][]string) map[string]string {
	rules := map[string]string{}
	for k, v := range chains {
		rules[k] = strings.Join(v, "\n") + "\n"
	}
	return rules
}

// applyDryRun turns the script's "would run" lines into the iptables -S
// output the commands would produce on freshHost.
func applyDryRun(t *testing.T, out string) map[string]string {
	t.Helper()
	chains := freshHost()
	simulate(t, chains, wouldRun(out))
	return toRules(chains)
}

// Regression: a re-install (every ah start) must never leave running egress
// sandboxes unfiltered, and an install that dies at any step must leave the
// bridge blocked. After every prefix of the second install's commands, the
// bridge is either cut off by the guard drops or fully filtered.
func TestSandboxEgressFirewallScript_ReinstallNeverOpensAWindow(t *testing.T) {
	out, err := runFirewallScript(t, "install", "--dry-run", "--no-persist", "--bridge", "ah-sbx-egress", "--host-ip", "203.0.113.7")
	require.NoError(t, err, out)
	lines := wouldRun(out)
	installed := freshHost()
	simulate(t, installed, lines)

	isBridgeDrop := func(f []string) bool {
		return flagValue(f, "-i") == "ah-sbx-egress" && flagValue(f, "-j") == "DROP" && isPlainRule(f, "-i")
	}
	guarded := 0
	for k := 0; k <= len(lines); k++ {
		chains := map[string][]string{}
		for key, v := range installed {
			chains[key] = append([]string(nil), v...)
		}
		simulate(t, chains, lines[:k])
		rules := toRules(chains)
		_, checkErr := checkSandboxEgressFirewall(context.Background(), "ah-sbx-egress", "linux", fakeRunner(rules))
		if checkErr == nil {
			continue
		}
		guarded++
		input := strings.Split(strings.TrimSpace(rules["iptables INPUT"]), "\n")
		dockerUser := strings.Split(strings.TrimSpace(rules["iptables DOCKER-USER"]), "\n")
		assert.True(t, ruleFirstEffective(input, isBridgeDrop), "after %d commands (%q) INPUT lets the bridge in: %v", k, lines[max(k-1, 0)], input)
		assert.True(t, ruleFirstEffective(dockerUser, isBridgeDrop), "after %d commands (%q) the bridge is neither filtered nor blocked: %v (%v)", k, lines[max(k-1, 0)], dockerUser, checkErr)
	}
	assert.Positive(t, guarded, "the rebuild does pass through partial states, all of them guarded")

	// The guards are gone once install completes.
	final := applyDryRun(t, out)
	for _, v := range final {
		assert.NotContains(t, v, "ah-sbx-egress-guard")
	}
}

func TestSandboxEgressFirewallScript_InstallDryRun(t *testing.T) {
	out, err := runFirewallScript(t, "install", "--dry-run", "--bridge", "ah-sbx-egress", "--host-ip", "203.0.113.7")
	require.NoError(t, err, out)

	for _, want := range []string{
		"would run: iptables -w -A AH-SBX-EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
		"would run: iptables -w -A AH-SBX-EGRESS -d 10.0.0.0/8 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 172.16.0.0/12 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 192.168.0.0/16 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 100.64.0.0/10 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 169.254.0.0/16 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 127.0.0.0/8 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 224.0.0.0/4 -j DROP",
		"would run: iptables -w -A AH-SBX-EGRESS -d 203.0.113.7/32 -j DROP",
		"would run: iptables -w -I DOCKER-USER 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS",
		"would run: iptables -w -I INPUT 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j DROP",
		"would run: ip6tables -w -I FORWARD 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j DROP",
		"would run: ip6tables -w -I INPUT 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j DROP",
		"would run: netfilter-persistent save",
		"dry-run: no changes made (bridge ah-sbx-egress)",
	} {
		assert.Contains(t, out, want)
	}
	// ESTABLISHED,RELATED comes first, the catch-all RETURN last.
	est := strings.Index(out, "--ctstate ESTABLISHED,RELATED -j RETURN")
	firstDrop := strings.Index(out, "-A AH-SBX-EGRESS -d ")
	last := strings.Index(out, "-A AH-SBX-EGRESS -j RETURN")
	assert.True(t, est < firstDrop && firstDrop < last, "rule order")
	// Guards go in before the chain is flushed and come out after the jump
	// and the INPUT drop are back.
	guardIn := strings.Index(out, "-I DOCKER-USER 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress-guard -j DROP")
	flush := strings.Index(out, "-F AH-SBX-EGRESS")
	jump := strings.Index(out, "-I DOCKER-USER 1 -i ah-sbx-egress -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS")
	guardOut := strings.Index(out, "-D DOCKER-USER -i ah-sbx-egress -m comment --comment ah-sbx-egress-guard -j DROP")
	assert.True(t, guardIn >= 0 && guardIn < flush && flush < jump && jump < guardOut, "guard order")

	// What the script installs is exactly what ah's startup check accepts.
	warnings, err := checkSandboxEgressFirewall(context.Background(), "ah-sbx-egress", "linux", fakeRunner(applyDryRun(t, out)))
	require.NoError(t, err)
	assert.Empty(t, warnings)

	noPersist, err := runFirewallScript(t, "install", "--dry-run", "--no-persist", "--host-ip", "203.0.113.7")
	require.NoError(t, err, noPersist)
	assert.NotContains(t, noPersist, "netfilter-persistent")
}

func TestSandboxEgressFirewallScript_RemoveDryRunAndErrors(t *testing.T) {
	out, err := runFirewallScript(t, "remove", "--dry-run", "--bridge", "sbx0")
	require.NoError(t, err, out)
	assert.Contains(t, out, "would run: iptables -w -D DOCKER-USER -i sbx0 -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS")
	assert.Contains(t, out, "would run: iptables -w -D INPUT -i sbx0 -m comment --comment ah-sbx-egress -j DROP")
	assert.Contains(t, out, "iptables -w -X AH-SBX-EGRESS")
	assert.NotContains(t, out, " -I ")

	for _, args := range [][]string{
		{},
		{"install", "--dry-run", "--bridge", "name-that-is-too-long"},
		{"install", "--dry-run", "--host-ip", "not-an-ip"},
		{"status", "--dry-run"},
		{"frobnicate"},
		{"install", "remove", "--dry-run"},
	} {
		out, err := runFirewallScript(t, args...)
		assert.Error(t, err, "%v accepted: %s", args, out)
	}

	out, err = runFirewallScript(t, "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Usage:")
}

func writeSnapshot(t *testing.T, rules map[string]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("# ah sandbox egress firewall snapshot v1 bridge=ah-sbx-egress\n")
	for _, k := range []string{"iptables FORWARD", "iptables DOCKER-USER", "iptables AH-SBX-EGRESS", "iptables INPUT", "ip6tables FORWARD", "ip6tables INPUT"} {
		b.WriteString("### " + k + "\n")
		if v, ok := rules[k]; ok {
			b.WriteString(v)
		} else {
			b.WriteString("#!unreadable\n")
		}
	}
	path := filepath.Join(t.TempDir(), "firewall.rules")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	return path
}

func TestFirewallSnapshotRunner(t *testing.T) {
	uid := uint32(os.Getuid())
	path := writeSnapshot(t, installedRules("ah-sbx-egress"))
	run, err := firewallSnapshotRunner(path, time.Now(), uid)
	require.NoError(t, err)
	warnings, err := checkSandboxEgressFirewall(context.Background(), "ah-sbx-egress", "linux", run)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	// Unreadable sections fail the check (here: no AH-SBX-EGRESS chain).
	rules := installedRules("ah-sbx-egress")
	delete(rules, "iptables AH-SBX-EGRESS")
	run, err = firewallSnapshotRunner(writeSnapshot(t, rules), time.Now(), uid)
	require.NoError(t, err)
	_, err = checkSandboxEgressFirewall(context.Background(), "ah-sbx-egress", "linux", run)
	require.Error(t, err)

	// Stale, wrongly owned, group-writable or missing snapshots are refused.
	_, err = firewallSnapshotRunner(path, time.Now().Add(time.Hour), uid)
	assert.ErrorContains(t, err, "old")
	_, err = firewallSnapshotRunner(path, time.Now(), uid+1)
	assert.ErrorContains(t, err, "owned by uid")
	require.NoError(t, os.Chmod(path, 0o664))
	_, err = firewallSnapshotRunner(path, time.Now(), uid)
	assert.ErrorContains(t, err, "writable")
	_, err = firewallSnapshotRunner(filepath.Join(t.TempDir(), "none"), time.Now(), uid)
	assert.ErrorIs(t, err, os.ErrNotExist)

	// No snapshot: fall back to iptables. A bad one disables egress.
	_, source, err := egressFirewallRunner(filepath.Join(t.TempDir(), "none"), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "iptables", source)
	_, _, err = egressFirewallRunner(path, time.Now())
	require.Error(t, err)
}

func TestSetupSandboxEgress_BadSnapshotDisablesEgress(t *testing.T) {
	path := writeSnapshot(t, installedRules("ah-sbx-egress"))
	require.NoError(t, os.Chmod(path, 0o666))
	net, lines, err := setupSandboxEgress(context.Background(), &fakeHardeningDocker{}, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", GOOS: "linux", SnapshotPath: path,
	})
	require.NoError(t, err)
	assert.Empty(t, net)
	assert.Contains(t, strings.Join(lines, "\n"), "writable by group or others")
}

func TestParseFirewallSnapshot(t *testing.T) {
	s := parseFirewallSnapshot("# header\n### iptables INPUT\n-P INPUT ACCEPT\n-A INPUT -i x -j DROP\n### ip6tables INPUT\n#!unreadable\n")
	assert.Equal(t, "-P INPUT ACCEPT\n-A INPUT -i x -j DROP\n", s["iptables INPUT"])
	_, ok := s["ip6tables INPUT"]
	assert.False(t, ok)
}

// fakeIptables is a minimal stateful iptables/ip6tables stand-in for running
// the script for real (not --dry-run): each chain is a file of "-A" lines.
const fakeIptables = `#!/usr/bin/env bash
set -u
state="$FAKE_IPT_DIR/$(basename "$0")"
mkdir -p "$state"
[[ ${1:-} == -w ]] && shift
op=$1 ch=$2; shift 2
f="$state/$ch"
case $op in
  -S) [[ -f $f ]] || exit 1; cat "$f" ;;
  -N) [[ -f $f ]] && exit 1; : > "$f" ;;
  -C) [[ -f $f ]] && grep -qxF -- "-A $ch $*" "$f" ;;
  -D) [[ -f $f ]] && grep -qxF -- "-A $ch $*" "$f" || exit 1
      RULE="-A $ch $*" awk '!d && $0 == ENVIRON["RULE"] {d=1; next} {print}' "$f" > "$f.tmp" && mv "$f.tmp" "$f" ;;
  -A) printf '%s\n' "-A $ch $*" >> "$f" ;;
  -I) shift; { printf '%s\n' "-A $ch $*"; cat "$f" 2>/dev/null || :; } > "$f.tmp" && mv "$f.tmp" "$f" ;;
  -F) [[ -f $f ]] || exit 1; : > "$f" ;;
  -X) [[ -f $f ]] || exit 1
      if grep -qsE -- "-j $ch( |$)" "$state"/*; then echo "iptables: Too many links." >&2; exit 1; fi
      rm -f "$f" ;;
  *) exit 2 ;;
esac
`

// Regression: removing the rules of one bridge (an old name after a rename,
// or a mistyped --bridge) must not flush the shared AH-SBX-EGRESS chain that
// the live bridge's DOCKER-USER jump still uses, which would leave the live
// bridge unfiltered.
func TestSandboxEgressFirewallScript_RemoveKeepsChainUsedByOtherBridge(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bin := t.TempDir()
	state := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "iptables"), []byte(fakeIptables), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ip6tables"), []byte(fakeIptables), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755))

	run := func(args ...string) string {
		cmd := exec.Command("bash", append([]string{firewallScript}, args...)...)
		cmd.Env = append(os.Environ(), "AH_SANDBOX_EGRESS_NETWORK=", "FAKE_IPT_DIR="+state,
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	chainRules := func() string {
		b, err := os.ReadFile(filepath.Join(state, "iptables", "AH-SBX-EGRESS"))
		require.NoError(t, err, "chain must still exist")
		return string(b)
	}

	run("install", "--no-persist", "--bridge", "sbx-old", "--host-ip", "203.0.113.7")
	run("install", "--no-persist", "--bridge", "sbx-live", "--host-ip", "203.0.113.7")

	out := run("remove", "--no-persist", "--bridge", "sbx-old")
	assert.Contains(t, out, "still used by another bridge")
	assert.Contains(t, chainRules(), "-A AH-SBX-EGRESS -d 169.254.0.0/16 -j DROP", "the live bridge stays filtered")
	du, err := os.ReadFile(filepath.Join(state, "iptables", "DOCKER-USER"))
	require.NoError(t, err)
	assert.Contains(t, string(du), "-i sbx-live -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS")
	assert.NotContains(t, string(du), "sbx-old")

	// Removing the last bridge deletes the chain.
	run("remove", "--no-persist", "--bridge", "sbx-live")
	_, err = os.Stat(filepath.Join(state, "iptables", "AH-SBX-EGRESS"))
	assert.True(t, os.IsNotExist(err), "chain removed once unused")
}
