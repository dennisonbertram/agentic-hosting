package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/config"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHardeningDocker struct {
	runtimes   map[string]docker.RuntimeInfo
	infoErr    error
	ensureErr  error
	ensured    []string // "name subnet"
	lookups    []string
	ensureHits int
}

func (f *fakeHardeningDocker) EnsureSandboxEgressNetwork(_ context.Context, name, subnet string) (string, error) {
	f.ensureHits++
	f.ensured = append(f.ensured, name+" "+subnet)
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	return "net-id", nil
}

func (f *fakeHardeningDocker) LookupRuntime(_ context.Context, name string) (docker.RuntimeInfo, bool, error) {
	f.lookups = append(f.lookups, name)
	if f.infoErr != nil {
		return docker.RuntimeInfo{}, false, f.infoErr
	}
	rt, ok := f.runtimes[name]
	return rt, ok, nil
}

type fakeTarget struct {
	egress  string
	runtime string
	tmpfs   bool
	calls   int
	// sweptWith records the egress network each delete-if-disabled call saw.
	sweptWith []string
	sweepN    int
	sweepErr  error
}

func (t *fakeTarget) DeleteEgressSandboxesIfDisabled(context.Context) (int, error) {
	t.sweptWith = append(t.sweptWith, t.egress)
	return t.sweepN, t.sweepErr
}

func (t *fakeTarget) SetSandboxEgressNetwork(n string) { t.egress = n; t.calls++ }
func (t *fakeTarget) SetSandboxRuntime(r string)       { t.runtime = r }
func (t *fakeTarget) SetSandboxWorkspaceTmpfs(on bool) { t.tmpfs = on }

// installedRules is iptables -S output with the firewall script's rules.
func installedRules(bridge string) map[string]string {
	return map[string]string{
		"iptables DOCKER-USER": "-N DOCKER-USER\n-A DOCKER-USER -i " + bridge + " -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS\n-A DOCKER-USER -j RETURN\n",
		"iptables AH-SBX-EGRESS": "-N AH-SBX-EGRESS\n" +
			"-A AH-SBX-EGRESS -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN\n" +
			"-A AH-SBX-EGRESS -d 10.0.0.0/8 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 172.16.0.0/12 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 192.168.0.0/16 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 100.64.0.0/10 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 169.254.0.0/16 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 127.0.0.0/8 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 0.0.0.0/8 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 224.0.0.0/4 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 240.0.0.0/4 -j DROP\n" +
			"-A AH-SBX-EGRESS -d 203.0.113.7/32 -j DROP\n" +
			"-A AH-SBX-EGRESS -j RETURN\n",
		"iptables FORWARD":  "-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n-A FORWARD -j DOCKER-FORWARD\n",
		"iptables INPUT":    "-P INPUT ACCEPT\n-A INPUT -i " + bridge + " -m comment --comment ah-sbx-egress -j DROP\n-A INPUT -p tcp --dport 22 -j ACCEPT\n",
		"ip6tables FORWARD": "-P FORWARD DROP\n-A FORWARD -i " + bridge + " -j DROP\n",
		"ip6tables INPUT":   "-P INPUT ACCEPT\n-A INPUT -i " + bridge + " -j DROP\n",
	}
}

func fakeRunner(rules map[string]string) commandRunner {
	return func(_ context.Context, name string, args ...string) (string, error) {
		chain := args[len(args)-1]
		out, ok := rules[name+" "+chain]
		if !ok {
			return "iptables: No chain/target/match by that name.", errors.New("exit status 1")
		}
		return out, nil
	}
}

func TestCheckSandboxRuntime(t *testing.T) {
	ctx := context.Background()
	d := &fakeHardeningDocker{runtimes: map[string]docker.RuntimeInfo{
		"runsc":       {Path: "runsc"},
		"runsc-sbx":   {Path: "/usr/local/bin/runsc", Args: []string{"--overlay2=root:memory"}},
		"runsc-plain": {Path: "/usr/local/bin/runsc"},
		"runc":        {Path: "runc"},
		"crun":        {Path: "/usr/bin/crun"},
	}}

	// Unset: nothing to check, Docker is not even asked.
	w, err := checkSandboxRuntime(ctx, d, "", false, false)
	require.NoError(t, err)
	assert.Empty(t, w)
	assert.Empty(t, d.lookups)

	// Registered gVisor runtime with the memory overlay: no warnings.
	w, err = checkSandboxRuntime(ctx, d, "runsc-sbx", false, true)
	require.NoError(t, err)
	assert.Empty(t, w)

	// Missing runtime refuses to start.
	_, err = checkSandboxRuntime(ctx, d, "runsc-missing", false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered with Docker")

	// docker info failure refuses to start.
	_, err = checkSandboxRuntime(ctx, &fakeHardeningDocker{infoErr: errors.New("daemon down")}, "runsc-sbx", false, true)
	require.Error(t, err)

	// runc needs --dev, like AH_CONTAINER_RUNTIME, and is checked before Docker.
	d.lookups = nil
	_, err = checkSandboxRuntime(ctx, d, "runc", false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --dev")
	assert.Empty(t, d.lookups)
	w, err = checkSandboxRuntime(ctx, d, "runc", true, true)
	require.NoError(t, err)
	require.NotEmpty(t, w)
	assert.Contains(t, w[0], "not gVisor")

	// Any non-gVisor runtime is refused outside --dev.
	_, err = checkSandboxRuntime(ctx, d, "crun", false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not gVisor")

	// gVisor without the memory overlay, and tmpfs off: warnings only.
	w, err = checkSandboxRuntime(ctx, d, "runsc-plain", false, false)
	require.NoError(t, err)
	require.Len(t, w, 2)
	assert.Contains(t, w[0], "--overlay2=root:memory")
	assert.Contains(t, w[1], "AH_SANDBOX_WORKSPACE_TMPFS")
}

func TestSetupSandboxEgress_FlagUnsetIsDisabled(t *testing.T) {
	d := &fakeHardeningDocker{}
	net, lines, err := setupSandboxEgress(context.Background(), d, sandboxEgressOptions{})
	require.NoError(t, err)
	assert.Empty(t, net)
	assert.Empty(t, lines)
	assert.Zero(t, d.ensureHits, "no network is created when egress is off")
}

func TestSetupSandboxEgress_InvalidConfigIsFatal(t *testing.T) {
	d := &fakeHardeningDocker{}
	_, _, err := setupSandboxEgress(context.Background(), d, sandboxEgressOptions{Network: "ah-tenant-evil", Subnet: config.DefaultSandboxEgressSubnet})
	require.Error(t, err)
	_, _, err = setupSandboxEgress(context.Background(), d, sandboxEgressOptions{Network: "ah-sbx-egress", Subnet: "10.0.0.0/8"})
	require.Error(t, err)
	assert.Zero(t, d.ensureHits)
}

func TestSetupSandboxEgress_FirewallVerified(t *testing.T) {
	d := &fakeHardeningDocker{}
	net, lines, err := setupSandboxEgress(context.Background(), d, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", GOOS: "linux", Run: fakeRunner(installedRules("ah-sbx-egress")),
	})
	require.NoError(t, err)
	assert.Equal(t, "ah-sbx-egress", net)
	assert.Equal(t, []string{"ah-sbx-egress 10.250.0.0/24"}, d.ensured)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "firewall rules verified")
}

func TestSetupSandboxEgress_FirewallMissingDisablesEgress(t *testing.T) {
	cases := map[string]func(r map[string]string){
		"no jump": func(r map[string]string) { r["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -j RETURN\n" },
		"jump for another bridge": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-A DOCKER-USER -i docker0 -j AH-SBX-EGRESS\n"
		},
		"accept before jump": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-A DOCKER-USER -j ACCEPT\n-A DOCKER-USER -i ah-sbx-egress -j AH-SBX-EGRESS\n"
		},
		"no chain": func(r map[string]string) { delete(r, "iptables AH-SBX-EGRESS") },
		"metadata not dropped": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = strings.Replace(r["iptables AH-SBX-EGRESS"], "-A AH-SBX-EGRESS -d 169.254.0.0/16 -j DROP\n", "", 1)
		},
		"drop narrowed to tcp": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = strings.Replace(r["iptables AH-SBX-EGRESS"], "-d 10.0.0.0/8 -j DROP", "-d 10.0.0.0/8 -p tcp -j DROP", 1)
		},
		"unconditional return before drops": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = "-A AH-SBX-EGRESS -j RETURN\n" + r["iptables AH-SBX-EGRESS"]
		},
		"accept before drops": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = "-A AH-SBX-EGRESS -p udp -j ACCEPT\n" + r["iptables AH-SBX-EGRESS"]
		},
		"no input drop": func(r map[string]string) { r["iptables INPUT"] = "-P INPUT ACCEPT\n" },
		"input accept first": func(r map[string]string) {
			r["iptables INPUT"] = "-A INPUT -p tcp --dport 22 -j ACCEPT\n-A INPUT -i ah-sbx-egress -j DROP\n"
		},
		// Regressions for the order checks: anything that can accept,
		// return or jump elsewhere ahead of a required rule fails.
		"forward does not jump to DOCKER-USER (nftables backend)": func(r map[string]string) {
			r["iptables FORWARD"] = "-P FORWARD ACCEPT\n"
		},
		"forward unreadable": func(r map[string]string) { delete(r, "iptables FORWARD") },
		"forward accepts before DOCKER-USER": func(r map[string]string) {
			r["iptables FORWARD"] = "-P FORWARD DROP\n-A FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n-A FORWARD -j DOCKER-USER\n"
		},
		"forward jumps to ufw chain before DOCKER-USER": func(r map[string]string) {
			r["iptables FORWARD"] = "-P FORWARD DROP\n-A FORWARD -j ufw-before-forward\n-A FORWARD -j DOCKER-USER\n"
		},
		"forward DOCKER-USER jump narrowed": func(r map[string]string) {
			r["iptables FORWARD"] = "-P FORWARD DROP\n-A FORWARD -p tcp -j DOCKER-USER\n"
		},
		"return before jump (ufw-docker)": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -s 10.0.0.0/8 -j RETURN\n-A DOCKER-USER -i ah-sbx-egress -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS\n"
		},
		"user chain jump before jump": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -j ufw-user-forward\n-A DOCKER-USER -i ah-sbx-egress -j AH-SBX-EGRESS\n"
		},
		// Regressions: a jump narrowed to one destination, or a drop
		// narrowed to another interface, is not the required rule.
		"jump narrowed to a destination": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -i ah-sbx-egress -d 203.0.113.9/32 -j AH-SBX-EGRESS\n"
		},
		"drop narrowed to an interface": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = strings.Replace(r["iptables AH-SBX-EGRESS"], "-A AH-SBX-EGRESS -d 10.0.0.0/8 -j DROP", "-A AH-SBX-EGRESS -i eth0 -d 10.0.0.0/8 -j DROP", 1)
		},
		"input drop narrowed to a destination": func(r map[string]string) {
			r["iptables INPUT"] = "-P INPUT ACCEPT\n-A INPUT -i ah-sbx-egress -d 10.250.0.1/32 -j DROP\n"
		},
		"jump narrowed to tcp": func(r map[string]string) {
			r["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -i ah-sbx-egress -p tcp -j AH-SBX-EGRESS\n"
		},
		"ctstate NEW return before drops": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = "-A AH-SBX-EGRESS -m conntrack --ctstate NEW -j RETURN\n" + r["iptables AH-SBX-EGRESS"]
		},
		"user chain jump before drops": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = "-A AH-SBX-EGRESS -j SOMETHING-ELSE\n" + r["iptables AH-SBX-EGRESS"]
		},
		"this-network not dropped": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = strings.Replace(r["iptables AH-SBX-EGRESS"], "-A AH-SBX-EGRESS -d 0.0.0.0/8 -j DROP\n", "", 1)
		},
		"reserved not dropped": func(r map[string]string) {
			r["iptables AH-SBX-EGRESS"] = strings.Replace(r["iptables AH-SBX-EGRESS"], "-A AH-SBX-EGRESS -d 240.0.0.0/4 -j DROP\n", "", 1)
		},
		"ufw input chain before input drop": func(r map[string]string) {
			r["iptables INPUT"] = "-P INPUT DROP\n-A INPUT -j ufw-before-input\n-A INPUT -i ah-sbx-egress -j DROP\n"
		},
		"iptables unavailable": func(r map[string]string) {
			for k := range r {
				delete(r, k)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rules := installedRules("ah-sbx-egress")
			mutate(rules)
			net, lines, err := setupSandboxEgress(context.Background(), &fakeHardeningDocker{}, sandboxEgressOptions{
				Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", GOOS: "linux", Run: fakeRunner(rules),
			})
			require.NoError(t, err, "a missing firewall is not fatal: ah runs with egress disabled")
			assert.Empty(t, net, "egress stays disabled")
			assert.Contains(t, strings.Join(lines, "\n"), "sandbox egress is DISABLED")
		})
	}
}

func TestSetupSandboxEgress_IPv6RulesOnlyWarn(t *testing.T) {
	rules := installedRules("ah-sbx-egress")
	delete(rules, "ip6tables FORWARD")
	rules["ip6tables INPUT"] = "-P INPUT ACCEPT\n"
	net, lines, err := setupSandboxEgress(context.Background(), &fakeHardeningDocker{}, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", GOOS: "linux", Run: fakeRunner(rules),
	})
	require.NoError(t, err)
	assert.Equal(t, "ah-sbx-egress", net)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "cannot read ip6tables FORWARD")
	assert.Contains(t, joined, "ip6tables INPUT has no")
}

func TestSetupSandboxEgress_NonLinuxDisablesEgress(t *testing.T) {
	net, lines, err := setupSandboxEgress(context.Background(), &fakeHardeningDocker{}, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", GOOS: "darwin", Run: fakeRunner(installedRules("ah-sbx-egress")),
	})
	require.NoError(t, err)
	assert.Empty(t, net)
	assert.Contains(t, strings.Join(lines, "\n"), "cannot verify the host firewall on darwin")
}

func TestSetupSandboxEgress_SkipFirewallCheck(t *testing.T) {
	ran := false
	net, lines, err := setupSandboxEgress(context.Background(), &fakeHardeningDocker{}, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", SkipFirewallCheck: true, GOOS: "darwin",
		Run: func(context.Context, string, ...string) (string, error) { ran = true; return "", nil },
	})
	require.NoError(t, err)
	assert.Equal(t, "ah-sbx-egress", net)
	assert.False(t, ran, "the check is skipped")
	assert.Contains(t, strings.Join(lines, "\n"), "WITHOUT verifying the host firewall")
}

func TestSetupSandboxEgress_NetworkErrorDisablesEgress(t *testing.T) {
	d := &fakeHardeningDocker{ensureErr: fmt.Errorf("existing network is internal")}
	net, lines, err := setupSandboxEgress(context.Background(), d, sandboxEgressOptions{
		Network: "ah-sbx-egress", Subnet: "10.250.0.0/24", SkipFirewallCheck: true,
	})
	require.NoError(t, err)
	assert.Empty(t, net)
	assert.Contains(t, strings.Join(lines, "\n"), "existing network is internal")
}

func TestConfigureSandboxHardening_DefaultsTouchNothing(t *testing.T) {
	d := &fakeHardeningDocker{}
	target := &fakeTarget{egress: "sentinel"}
	var logged []string
	err := configureSandboxHardening(context.Background(), config.Default(), "", false, d, target, sandboxEgressOptions{},
		func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	require.NoError(t, err)
	assert.Empty(t, target.egress)
	assert.Empty(t, target.runtime)
	assert.True(t, target.tmpfs, "the bounded tmpfs workspace is the default")
	assert.Empty(t, d.lookups, "no Docker calls with the defaults")
	assert.Zero(t, d.ensureHits)
	assert.Empty(t, logged)
}

func TestConfigureSandboxHardening_Applies(t *testing.T) {
	d := &fakeHardeningDocker{runtimes: map[string]docker.RuntimeInfo{
		"runsc-sbx": {Path: "/usr/local/bin/runsc", Args: []string{"--overlay2=root:memory"}},
	}}
	cfg := config.Default()
	cfg.SandboxRuntime = "runsc-sbx"
	cfg.SandboxWorkspaceTmpfs = true
	target := &fakeTarget{}
	err := configureSandboxHardening(context.Background(), cfg, "ah-sbx-egress", false, d, target,
		sandboxEgressOptions{GOOS: "linux", Run: fakeRunner(installedRules("ah-sbx-egress"))}, func(string, ...any) {})
	require.NoError(t, err)
	assert.Equal(t, "ah-sbx-egress", target.egress)
	assert.Equal(t, "runsc-sbx", target.runtime)
	assert.True(t, target.tmpfs)

	// A missing runtime is fatal and applies nothing.
	cfg.SandboxRuntime = "runsc-gone"
	target = &fakeTarget{}
	err = configureSandboxHardening(context.Background(), cfg, "", false, d, target, sandboxEgressOptions{}, func(string, ...any) {})
	require.Error(t, err)
	assert.Zero(t, target.calls)
	assert.Empty(t, target.sweptWith)
}

// Regression: when the firewall check disables egress, egress sandboxes from
// an earlier run are deleted after the (empty) network is applied.
func TestConfigureSandboxHardening_DisabledEgressDeletesEgressSandboxes(t *testing.T) {
	rules := installedRules("ah-sbx-egress")
	delete(rules, "iptables AH-SBX-EGRESS")
	target := &fakeTarget{egress: "sentinel", sweepN: 2, sweepErr: errors.New("daemon busy")}
	var logged []string
	err := configureSandboxHardening(context.Background(), config.Default(), "ah-sbx-egress", false, &fakeHardeningDocker{}, target,
		sandboxEgressOptions{GOOS: "linux", Run: fakeRunner(rules)},
		func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	require.NoError(t, err, "a failed delete is not fatal")
	assert.Equal(t, []string{""}, target.sweptWith, "runs once, after egress was set to disabled")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "deleted 2 sandbox(es)")
	assert.Contains(t, joined, "daemon busy")
}

func TestDefaultSandboxEgressSubnetMatches(t *testing.T) {
	assert.Equal(t, docker.DefaultSandboxEgressSubnet, config.DefaultSandboxEgressSubnet)
}

func TestIsPlainRule(t *testing.T) {
	assert.True(t, isPlainRule(strings.Fields("-A INPUT -i b -j DROP"), "-i"))
	assert.True(t, isPlainRule(strings.Fields("-A INPUT -i b -m comment --comment x -j DROP"), "-i"))
	assert.False(t, isPlainRule(strings.Fields("-A INPUT -i b -p tcp -j DROP"), "-i"))
	assert.False(t, isPlainRule(strings.Fields("-A INPUT -i b -m tcp -j DROP"), "-i"))
	assert.False(t, isPlainRule(strings.Fields("-A INPUT -i"), "-i"))
	// Regression: each check allows only its own selector.
	assert.False(t, isPlainRule(strings.Fields("-A DOCKER-USER -i b -d 203.0.113.9/32 -j AH-SBX-EGRESS"), "-i"))
	assert.True(t, isPlainRule(strings.Fields("-A AH-SBX-EGRESS -d 10.0.0.0/8 -j DROP"), "-d"))
	assert.False(t, isPlainRule(strings.Fields("-A AH-SBX-EGRESS -i eth0 -d 10.0.0.0/8 -j DROP"), "-d"))
}

func TestFirewallOrder_DropsAndLogsAheadAreFine(t *testing.T) {
	rules := installedRules("ah-sbx-egress")
	rules["iptables INPUT"] = "-P INPUT ACCEPT\n-A INPUT -s 198.51.100.9/32 -j REJECT --reject-with icmp-port-unreachable\n-A INPUT -j LOG\n-A INPUT -i ah-sbx-egress -j DROP\n"
	rules["iptables DOCKER-USER"] = "-N DOCKER-USER\n-A DOCKER-USER -s 198.51.100.9/32 -j DROP\n-A DOCKER-USER -i ah-sbx-egress -m comment --comment ah-sbx-egress-guard -j DROP\n-A DOCKER-USER -i ah-sbx-egress -m comment --comment ah-sbx-egress -j AH-SBX-EGRESS\n"
	_, err := checkSandboxEgressFirewall(context.Background(), "ah-sbx-egress", "linux", fakeRunner(rules))
	require.NoError(t, err)
}

func TestIsEstablishedReturn(t *testing.T) {
	assert.True(t, isEstablishedReturn(strings.Fields("-A C -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")))
	assert.True(t, isEstablishedReturn(strings.Fields("-A C -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN")))
	assert.False(t, isEstablishedReturn(strings.Fields("-A C -m conntrack --ctstate NEW -j RETURN")))
	assert.False(t, isEstablishedReturn(strings.Fields("-A C -m conntrack --ctstate NEW,RELATED,ESTABLISHED -j RETURN")))
	assert.False(t, isEstablishedReturn(strings.Fields("-A C -s 10.0.0.0/8 -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")))
	assert.False(t, isEstablishedReturn(strings.Fields("-A C -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT")))
}
