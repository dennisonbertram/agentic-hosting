package docker

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
)

// Sandbox egress network. Sandboxes created with egress "internet" attach
// only to this one shared bridge. Unlike tenant networks it is not internal,
// so containers on it can reach the internet through Docker's masquerade;
// host firewall rules (docs/ops/sandbox-egress-firewall.sh) stop them from
// reaching private ranges, link-local metadata and the host itself.
const (
	// DefaultSandboxEgressSubnet is the egress bridge subnet when the operator
	// does not set one. It lies outside Docker's default address pools
	// (172.17.0.0/12 and 192.168.0.0/16) so it does not collide with tenant
	// networks that Docker allocates automatically.
	DefaultSandboxEgressSubnet = "10.250.0.0/24"

	// SandboxEgressLabel marks the egress bridge as owned by ah. GC only
	// removes ah-tenant-*, ah-ingress-* and ah-dbnet-* networks, and the
	// name rules below keep the egress network out of those prefixes.
	SandboxEgressLabel      = "ah.managed"
	SandboxEgressLabelValue = "sandbox-egress"
)

// egressNetworkNamePattern limits the network name to a valid Linux
// interface name (at most 15 bytes), because the bridge interface takes the
// same name and the firewall rules match on it.
var egressNetworkNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

// gcManagedNetworkPrefixes are network name prefixes that GC may remove.
var gcManagedNetworkPrefixes = []string{"ah-tenant-", "ah-ingress-", "ah-dbnet-", "ah-pool"}

// ValidateSandboxEgressNetwork checks the operator's egress network name and
// subnet. The name doubles as the bridge interface name, so it must be a
// valid interface name; it must not collide with networks GC manages or with
// Docker's built-in networks. The subnet must be an IPv4 prefix no larger
// than /16 and no smaller than /29.
func ValidateSandboxEgressNetwork(name, subnet string) error {
	if !egressNetworkNamePattern.MatchString(name) {
		return fmt.Errorf("sandbox egress network %q must be 1-15 characters of letters, digits, '_', '.', '-' (it is also the bridge interface name)", name)
	}
	switch name {
	case "bridge", "host", "none", "docker0":
		return fmt.Errorf("sandbox egress network %q must not be a built-in Docker network", name)
	}
	for _, p := range gcManagedNetworkPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("sandbox egress network %q must not start with %q (reserved for networks GC manages)", name, p)
		}
	}
	pfx, err := netip.ParsePrefix(subnet)
	if err != nil {
		return fmt.Errorf("sandbox egress subnet %q: %w", subnet, err)
	}
	if !pfx.Addr().Is4() {
		return fmt.Errorf("sandbox egress subnet %q must be IPv4 (IPv6 is disabled on the egress bridge)", subnet)
	}
	if pfx.Bits() < 16 || pfx.Bits() > 29 {
		return fmt.Errorf("sandbox egress subnet %q must be between /16 and /29", subnet)
	}
	if pfx.Masked() != pfx {
		return fmt.Errorf("sandbox egress subnet %q has host bits set (use %s)", subnet, pfx.Masked())
	}
	return nil
}

// sandboxEgressNetworkCreateOptions is the egress bridge: not internal (it
// must reach the internet), ICC off (sandboxes cannot reach each other),
// IPv6 off (the firewall rules are IPv4), a fixed bridge interface name the
// firewall rules match on, a fixed subnet and the ah ownership label.
func sandboxEgressNetworkCreateOptions(name, subnet string) network.CreateOptions {
	ipv6 := false
	return network.CreateOptions{
		Driver:     "bridge",
		Internal:   false,
		EnableIPv6: &ipv6,
		IPAM: &network.IPAM{
			Driver: "default",
			Config: []network.IPAMConfig{{Subnet: subnet}},
		},
		Options: map[string]string{
			"com.docker.network.bridge.name":                 name,
			"com.docker.network.bridge.enable_icc":           "false",
			"com.docker.network.bridge.enable_ip_masquerade": "true",
		},
		Labels: map[string]string{SandboxEgressLabel: SandboxEgressLabelValue},
	}
}

// checkSandboxEgressNetwork verifies an existing network has the egress
// settings. An operator-made or stale network with different settings (for
// example ICC on, IPv6 on, or another bridge name the firewall rules would
// not match) is refused rather than reused.
func checkSandboxEgressNetwork(n network.Inspect, name, subnet string) error {
	var problems []string
	if n.Driver != "bridge" {
		problems = append(problems, fmt.Sprintf("driver is %q, want bridge", n.Driver))
	}
	if n.Internal {
		problems = append(problems, "network is internal (no egress)")
	}
	if n.EnableIPv6 {
		problems = append(problems, "IPv6 is enabled")
	}
	if n.Options["com.docker.network.bridge.enable_icc"] != "false" {
		problems = append(problems, "inter-container communication is not disabled")
	}
	if n.Options["com.docker.network.bridge.name"] != name {
		problems = append(problems, fmt.Sprintf("bridge interface is %q, want %q", n.Options["com.docker.network.bridge.name"], name))
	}
	if n.Labels[SandboxEgressLabel] != SandboxEgressLabelValue {
		problems = append(problems, fmt.Sprintf("missing label %s=%s", SandboxEgressLabel, SandboxEgressLabelValue))
	}
	subnetOK := false
	for _, c := range n.IPAM.Config {
		if c.Subnet == subnet {
			subnetOK = true
		}
	}
	if !subnetOK {
		problems = append(problems, fmt.Sprintf("subnet is not %s", subnet))
	}
	if len(problems) > 0 {
		return fmt.Errorf("existing network %q is not a usable sandbox egress network: %s; remove it (docker network rm %s) and restart ah",
			name, strings.Join(problems, "; "), name)
	}
	return nil
}

// EnsureSandboxEgressNetwork creates the sandbox egress bridge, or checks
// that an existing network with that name has the egress settings. It
// returns the network ID.
func (c *DockerClient) EnsureSandboxEgressNetwork(ctx context.Context, name, subnet string) (string, error) {
	if err := ValidateSandboxEgressNetwork(name, subnet); err != nil {
		return "", err
	}
	n, err := c.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		if err := checkSandboxEgressNetwork(n, name, subnet); err != nil {
			return "", err
		}
		return n.ID, nil
	}
	if !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("inspect network %s: %w", name, err)
	}
	resp, err := c.cli.NetworkCreate(ctx, name, sandboxEgressNetworkCreateOptions(name, subnet))
	if err != nil {
		return "", fmt.Errorf("create sandbox egress network %s: %w", name, err)
	}
	return resp.ID, nil
}

// RuntimeInfo describes an OCI runtime registered with the Docker daemon.
type RuntimeInfo struct {
	Path string
	Args []string
}

// LookupRuntime returns the named runtime from `docker info`. found is false
// when the daemon has no runtime with that name.
func (c *DockerClient) LookupRuntime(ctx context.Context, name string) (RuntimeInfo, bool, error) {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return RuntimeInfo{}, false, fmt.Errorf("docker info: %w", err)
	}
	rt, ok := info.Runtimes[name]
	if !ok {
		return RuntimeInfo{}, false, nil
	}
	return RuntimeInfo{Path: rt.Path, Args: append([]string(nil), rt.Args...)}, true, nil
}

// IsGVisorRuntime reports whether a registered runtime runs gVisor: its
// binary is runsc (any directory), or the runtime is the built-in name runsc
// with no explicit path.
func IsGVisorRuntime(name string, rt RuntimeInfo) bool {
	if rt.Path == "" {
		return name == DefaultRuntime
	}
	return filepath.Base(rt.Path) == "runsc"
}
