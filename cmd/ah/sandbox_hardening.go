package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/config"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
)

// Sandbox hardening (phase 2) startup checks. Both features are opt-in and
// fail closed: a sandbox runtime that is configured but missing stops ah
// from starting; an egress network whose host firewall rules cannot be
// verified leaves egress disabled (egress "internet" keeps returning 403).

// sandboxEgressChain is the iptables chain docs/ops/sandbox-egress-firewall.sh
// installs; DOCKER-USER jumps to it for traffic from the egress bridge.
const sandboxEgressChain = "AH-SBX-EGRESS"

// sandboxEgressDropCIDRs are the destinations the egress chain must drop:
// RFC 1918, carrier-grade NAT, link-local (cloud metadata), loopback, "this
// network", multicast and reserved/broadcast: the same list as drop_cidrs in
// the script. The script also drops the host's own public IPs, but those are
// host-specific; the INPUT drop covers them for traffic to the host.
var sandboxEgressDropCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"0.0.0.0/8",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

// sandboxHardeningDocker is the Docker access the startup checks need.
type sandboxHardeningDocker interface {
	EnsureSandboxEgressNetwork(ctx context.Context, name, subnet string) (string, error)
	LookupRuntime(ctx context.Context, name string) (docker.RuntimeInfo, bool, error)
}

// commandRunner runs a host command and returns its combined output.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

func runHostCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// checkSandboxRuntime validates AH_SANDBOX_RUNTIME. Empty is fine (the
// container runtime is used). Like AH_CONTAINER_RUNTIME, runc needs --dev;
// any runtime must be registered with Docker, and outside --dev it must be a
// gVisor (runsc) binary. The returned lines are warnings to log; an error
// means ah must not start.
func checkSandboxRuntime(ctx context.Context, d sandboxHardeningDocker, name string, devMode, workspaceTmpfs bool) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	if name == config.RuntimeRunc && !devMode {
		return nil, fmt.Errorf("AH_SANDBOX_RUNTIME=runc is a dev-only fallback and requires --dev; production sandboxes must run under gVisor (runsc)")
	}
	rt, found, err := d.LookupRuntime(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("AH_SANDBOX_RUNTIME=%s: cannot list Docker runtimes: %w", name, err)
	}
	if !found {
		return nil, fmt.Errorf("AH_SANDBOX_RUNTIME=%s is not registered with Docker (see `docker info --format '{{json .Runtimes}}'`); register it in /etc/docker/daemon.json (setup-linux.sh --sandbox-hardening, docs/runbooks/sandboxes-hardening.md) or unset AH_SANDBOX_RUNTIME", name)
	}
	var warnings []string
	if !docker.IsGVisorRuntime(name, rt) {
		what := rt.Path
		if what == "" {
			what = "runtimeType " + rt.Type
		}
		if !devMode {
			return nil, fmt.Errorf("AH_SANDBOX_RUNTIME=%s runs %q, which is not gVisor (runsc); production sandboxes must run under gVisor", name, what)
		}
		warnings = append(warnings, fmt.Sprintf("WARNING: AH_SANDBOX_RUNTIME=%s (%s) is not gVisor; sandboxes share the host kernel (dev only)", name, what))
		return warnings, nil
	}
	if !hasArg(rt.Args, "--overlay2=root:memory") {
		warnings = append(warnings, fmt.Sprintf("WARNING: AH_SANDBOX_RUNTIME=%s is not registered with --overlay2=root:memory; writes to the sandbox root filesystem are not bounded by its memory limit", name))
	}
	if !workspaceTmpfs {
		warnings = append(warnings, "WARNING: AH_SANDBOX_WORKSPACE_TMPFS is off; sandbox /workspace is a volume with no size limit")
	}
	return warnings, nil
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// sandboxEgressOptions configures setupSandboxEgress.
type sandboxEgressOptions struct {
	Network           string
	Subnet            string
	SkipFirewallCheck bool
	GOOS              string        // runtime.GOOS unless a test overrides it
	Run               commandRunner // nil: the firewall snapshot if present, else iptables
	SnapshotPath      string        // "" = defaultFirewallSnapshotPath
}

// setupSandboxEgress returns the egress network to enable, or "" to leave
// egress disabled. An invalid name or subnet is a configuration error (ah
// must not start). A network that cannot be created or reused, or host
// firewall rules that cannot be verified, only disable egress: the lines
// returned explain why, loudly, and the rest of ah runs normally.
func setupSandboxEgress(ctx context.Context, d sandboxHardeningDocker, opts sandboxEgressOptions) (string, []string, error) {
	if opts.Network == "" {
		return "", nil, nil
	}
	if err := docker.ValidateSandboxEgressNetwork(opts.Network, opts.Subnet); err != nil {
		return "", nil, fmt.Errorf("AH_SANDBOX_EGRESS_NETWORK/AH_SANDBOX_EGRESS_SUBNET: %w", err)
	}
	disabled := func(reason string) []string {
		return []string{
			"################################################################",
			"WARNING: sandbox egress is DISABLED: " + reason,
			"WARNING: egress \"internet\" requests get 403 until this is fixed and ah restarts.",
			"WARNING: see docs/runbooks/sandboxes-hardening.md",
			"################################################################",
		}
	}
	if _, err := d.EnsureSandboxEgressNetwork(ctx, opts.Network, opts.Subnet); err != nil {
		return "", disabled(err.Error()), nil
	}
	if opts.SkipFirewallCheck {
		return opts.Network, []string{
			"################################################################",
			"WARNING: AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK=1: sandbox egress is enabled on " + opts.Network,
			"WARNING: WITHOUT verifying the host firewall. Without the rules from",
			"WARNING: docs/ops/sandbox-egress-firewall.sh, egress sandboxes can reach",
			"WARNING: private networks, cloud metadata (169.254.169.254) and this host.",
			"################################################################",
		}, nil
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	run, source := opts.Run, "iptables"
	if run == nil {
		snapshot := opts.SnapshotPath
		if snapshot == "" {
			snapshot = defaultFirewallSnapshotPath
		}
		var err error
		if run, source, err = egressFirewallRunner(snapshot, time.Now()); err != nil {
			return "", disabled(err.Error()), nil
		}
	}
	warnings, err := checkSandboxEgressFirewall(ctx, opts.Network, goos, run)
	if err != nil {
		return "", append(warnings, disabled(err.Error())...), nil
	}
	return opts.Network, append(warnings, fmt.Sprintf("sandboxes: egress enabled on %s (%s); host firewall rules verified (%s)", opts.Network, opts.Subnet, source)), nil
}

// checkSandboxEgressFirewall verifies (best effort) that the rules from
// docs/ops/sandbox-egress-firewall.sh are installed for bridge:
//   - FORWARD jumps to DOCKER-USER (Docker's iptables backend is in use);
//   - DOCKER-USER jumps to AH-SBX-EGRESS for -i bridge, unnarrowed;
//   - AH-SBX-EGRESS drops every sandboxEgressDropCIDRs destination, with
//     nothing ahead of those drops that could accept or return a new
//     connection (only the exact RELATED,ESTABLISHED RETURN is allowed);
//   - INPUT drops -i bridge.
//
// Each of these jumps/drops must come before any rule of its chain that is
// not a drop, reject or log (see ruleFirstEffective).
//
// IPv6 is off on the egress bridge, so missing ip6tables rules only warn.
// It reads rules with `iptables -S`, so ah must run as root on Linux.
func checkSandboxEgressFirewall(ctx context.Context, bridge, goos string, run commandRunner) ([]string, error) {
	if goos != "linux" {
		return nil, fmt.Errorf("cannot verify the host firewall on %s (Linux iptables only); set AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK=1 only if egress is filtered some other way", goos)
	}
	read := func(bin, chain string) ([]string, error) {
		out, err := run(ctx, bin, "-w", "-S", chain)
		if err != nil {
			return nil, fmt.Errorf("%s -S %s: %v: %s", bin, chain, err, strings.TrimSpace(out))
		}
		var rules []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "-A "+chain+" ") {
				rules = append(rules, line)
			}
		}
		return rules, nil
	}
	missing := func(what string) error {
		return fmt.Errorf("host firewall rule missing or out of order: %s; run docs/ops/sandbox-egress-firewall.sh install --bridge %s", what, bridge)
	}

	// Docker hooks DOCKER-USER into FORWARD only with its iptables backend;
	// with firewall-backend nftables or "iptables": false nothing jumps to
	// it, and the egress chain would filter nothing.
	forward, err := read("iptables", "FORWARD")
	if err != nil {
		return nil, err
	}
	if !ruleFirstEffective(forward, func(f []string) bool {
		return len(f) == 4 && flagValue(f, "-j") == "DOCKER-USER"
	}) {
		return nil, fmt.Errorf("host firewall rule missing or out of order: FORWARD -j DOCKER-USER (Docker must manage iptables with its iptables firewall backend, and nothing may accept, return or jump elsewhere ahead of it)")
	}

	dockerUser, err := read("iptables", "DOCKER-USER")
	if err != nil {
		return nil, err
	}
	if !ruleFirstEffective(dockerUser, func(f []string) bool {
		return flagValue(f, "-i") == bridge && flagValue(f, "-j") == sandboxEgressChain && isPlainRule(f)
	}) {
		return nil, missing(fmt.Sprintf("DOCKER-USER -i %s -j %s", bridge, sandboxEgressChain))
	}

	chain, err := read("iptables", sandboxEgressChain)
	if err != nil {
		return nil, err
	}
	drops := map[string]int{}
	firstLoose := -1 // first rule that can let a packet skip the drops below it
	for i, line := range chain {
		f := strings.Fields(line)
		target := flagValue(f, "-j")
		if target == "DROP" && flagValue(f, "-d") != "" && isPlainRule(f) {
			// "-A CHAIN -d CIDR -j DROP": an unconditional drop of that destination.
			if _, seen := drops[flagValue(f, "-d")]; !seen {
				drops[flagValue(f, "-d")] = i
			}
		}
		if firstLoose < 0 && !isHarmlessRule(f) && !isEstablishedReturn(f) {
			firstLoose = i
		}
	}
	for _, cidr := range sandboxEgressDropCIDRs {
		at, ok := drops[cidr]
		if !ok || (firstLoose >= 0 && firstLoose < at) {
			return nil, missing(fmt.Sprintf("%s -d %s -j DROP", sandboxEgressChain, cidr))
		}
	}

	input, err := read("iptables", "INPUT")
	if err != nil {
		return nil, err
	}
	if !ruleFirstEffective(input, func(f []string) bool {
		return flagValue(f, "-i") == bridge && flagValue(f, "-j") == "DROP" && isPlainRule(f)
	}) {
		return nil, missing(fmt.Sprintf("INPUT -i %s -j DROP", bridge))
	}

	var warnings []string
	for _, chain := range []string{"FORWARD", "INPUT"} {
		rules, err := read("ip6tables", chain)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("WARNING: cannot read ip6tables %s (%v); IPv6 is disabled on %s, so egress stays IPv4 only", chain, err, bridge))
			continue
		}
		if !ruleFirstEffective(rules, func(f []string) bool {
			return flagValue(f, "-i") == bridge && flagValue(f, "-j") == "DROP" && isPlainRule(f)
		}) {
			warnings = append(warnings, fmt.Sprintf("WARNING: ip6tables %s has no '-i %s -j DROP' rule (defence in depth; IPv6 is disabled on the bridge)", chain, bridge))
		}
	}
	return warnings, nil
}

// ruleFirstEffective reports whether a rule matching want comes before any
// rule that could let a packet leave the chain without reaching it: only
// rules that drop, reject or merely log or count may come first. An ACCEPT,
// a RETURN, or a jump or goto to another chain (which may accept or return,
// e.g. ufw-before-input or a ufw-docker "-s 10.0.0.0/8 -j RETURN") ahead of
// it means some traffic skips it.
func ruleFirstEffective(rules []string, want func(fields []string) bool) bool {
	for _, line := range rules {
		f := strings.Fields(line)
		if want(f) {
			return true
		}
		if !isHarmlessRule(f) {
			return false
		}
	}
	return false
}

// isHarmlessRule reports whether an iptables -S rule can only drop the
// packet or let it continue to the next rule: a DROP, REJECT, LOG or NFLOG
// target, or no target at all (a counter).
func isHarmlessRule(fields []string) bool {
	if flagValue(fields, "-g") != "" {
		return false
	}
	switch flagValue(fields, "-j") {
	case "", "DROP", "REJECT", "LOG", "NFLOG":
		return true
	}
	return false
}

// isEstablishedReturn reports whether the rule is exactly the egress chain's
// "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN" (either order of the
// two states): replies to connections the sandbox may already have.
func isEstablishedReturn(fields []string) bool {
	if len(fields) != 8 || fields[0] != "-A" || fields[2] != "-m" || fields[3] != "conntrack" ||
		fields[4] != "--ctstate" || fields[6] != "-j" || fields[7] != "RETURN" {
		return false
	}
	return fields[5] == "RELATED,ESTABLISHED" || fields[5] == "ESTABLISHED,RELATED"
}

// isPlainRule reports whether an iptables -S rule ("-A CHAIN ...") matches
// only on the input interface and/or destination (plus an optional comment),
// so it is not narrowed to a protocol, port or source.
func isPlainRule(fields []string) bool {
	if len(fields) < 2 || len(fields)%2 != 0 {
		return false
	}
	for i := 2; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "-i", "-d", "-j", "--comment":
		case "-m":
			if fields[i+1] != "comment" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// flagValue returns the token after flag in an iptables -S rule, or "".
func flagValue(fields []string, flag string) string {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == flag {
			return fields[i+1]
		}
	}
	return ""
}

// sandboxHardeningTarget is the environments manager surface main wires.
type sandboxHardeningTarget interface {
	SetSandboxEgressNetwork(name string)
	SetSandboxRuntime(runtime string)
	SetSandboxWorkspaceTmpfs(enabled bool)
	DeleteEgressSandboxesIfDisabled(ctx context.Context) (int, error)
}

// configureSandboxHardening runs the startup checks and applies the result.
// logf receives warnings; a returned error is fatal.
func configureSandboxHardening(ctx context.Context, cfg config.Config, egressNetwork string, devMode bool, d sandboxHardeningDocker, target sandboxHardeningTarget, egressOpts sandboxEgressOptions, logf func(string, ...any)) error {
	warnings, err := checkSandboxRuntime(ctx, d, cfg.SandboxRuntime, devMode, cfg.SandboxWorkspaceTmpfs)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logf("%s", w)
	}
	egressOpts.Network = egressNetwork
	egressOpts.Subnet = cfg.SandboxEgressSubnet
	egressOpts.SkipFirewallCheck = cfg.SandboxEgressSkipFirewallCheck
	enabled, lines, err := setupSandboxEgress(ctx, d, egressOpts)
	if err != nil {
		return err
	}
	for _, l := range lines {
		logf("%s", l)
	}
	target.SetSandboxRuntime(cfg.SandboxRuntime)
	target.SetSandboxWorkspaceTmpfs(cfg.SandboxWorkspaceTmpfs)
	target.SetSandboxEgressNetwork(enabled)
	// Egress sandboxes from an earlier run must not outlive the firewall
	// check: with egress disabled they are deleted. Not fatal; a failed
	// teardown stays 'deleting' and the reaper retries it.
	n, err := target.DeleteEgressSandboxesIfDisabled(ctx)
	if n > 0 {
		logf("sandboxes: egress is disabled; deleted %d sandbox(es) with egress \"internet\"", n)
	}
	if err != nil {
		logf("WARNING: sandboxes: egress is disabled but not every egress sandbox could be deleted: %v", err)
	}
	return nil
}
