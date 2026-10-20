package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSandboxEgressNetwork(t *testing.T) {
	require.NoError(t, ValidateSandboxEgressNetwork("ah-sbx-egress", DefaultSandboxEgressSubnet))
	require.NoError(t, ValidateSandboxEgressNetwork("sbx0", "172.29.8.0/22"))

	for _, tc := range []struct{ name, subnet, want string }{
		{"", DefaultSandboxEgressSubnet, "1-15 characters"},
		{"ah-sbx-egress-too-long", DefaultSandboxEgressSubnet, "1-15 characters"},
		{"bad name", DefaultSandboxEgressSubnet, "1-15 characters"},
		{"bridge", DefaultSandboxEgressSubnet, "built-in"},
		{"ah-tenant-x", DefaultSandboxEgressSubnet, "reserved"},
		{"ah-dbnet-x", DefaultSandboxEgressSubnet, "reserved"},
		{"ah-pool", DefaultSandboxEgressSubnet, "reserved"},
		{"ah-sbx-egress", "nope", "subnet"},
		{"ah-sbx-egress", "fd00::/64", "IPv4"},
		{"ah-sbx-egress", "10.0.0.0/8", "between /16 and /29"},
		{"ah-sbx-egress", "10.250.0.0/30", "between /16 and /29"},
		{"ah-sbx-egress", "10.250.0.1/24", "host bits"},
	} {
		err := ValidateSandboxEgressNetwork(tc.name, tc.subnet)
		require.Error(t, err, "%s %s", tc.name, tc.subnet)
		assert.Contains(t, err.Error(), tc.want, "%s %s", tc.name, tc.subnet)
	}
}

func TestEnsureSandboxEgressNetwork_CreatesHardenedBridge(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("POST /networks/create", 201, map[string]any{"Id": "egress-1"})

	id, err := c.EnsureSandboxEgressNetwork(context.Background(), "ah-sbx-egress", "10.250.0.0/24")
	require.NoError(t, err)
	assert.Equal(t, "egress-1", id)

	var opts network.CreateRequest
	require.NoError(t, json.Unmarshal(f.bodies["POST /networks/create"], &opts))
	assert.Equal(t, "ah-sbx-egress", opts.Name)
	assert.Equal(t, "bridge", opts.Driver)
	assert.False(t, opts.Internal, "the egress bridge must reach the internet")
	require.NotNil(t, opts.EnableIPv6)
	assert.False(t, *opts.EnableIPv6, "IPv6 off: the firewall rules are IPv4 only")
	assert.Equal(t, "false", opts.Options["com.docker.network.bridge.enable_icc"])
	assert.Equal(t, "ah-sbx-egress", opts.Options["com.docker.network.bridge.name"], "fixed interface name for the firewall rules")
	assert.Equal(t, SandboxEgressLabelValue, opts.Labels[SandboxEgressLabel])
	require.NotNil(t, opts.IPAM)
	require.Len(t, opts.IPAM.Config, 1)
	assert.Equal(t, "10.250.0.0/24", opts.IPAM.Config[0].Subnet)
}

func goodEgressInspect() map[string]any {
	return map[string]any{
		"Name": "ah-sbx-egress", "Id": "egress-existing", "Driver": "bridge",
		"Internal": false, "EnableIPv6": false,
		"IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "10.250.0.0/24"}}},
		"Options": map[string]string{
			"com.docker.network.bridge.name":       "ah-sbx-egress",
			"com.docker.network.bridge.enable_icc": "false",
		},
		"Labels": map[string]string{SandboxEgressLabel: SandboxEgressLabelValue},
	}
}

func TestEnsureSandboxEgressNetwork_ReusesMatchingNetwork(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /networks/ah-sbx-egress", 200, goodEgressInspect())

	id, err := c.EnsureSandboxEgressNetwork(context.Background(), "ah-sbx-egress", "10.250.0.0/24")
	require.NoError(t, err)
	assert.Equal(t, "egress-existing", id)
	assert.False(t, f.called("POST /networks/create"))
}

func TestEnsureSandboxEgressNetwork_RefusesMismatchedNetwork(t *testing.T) {
	for name, mutate := range map[string]func(m map[string]any){
		"internal": func(m map[string]any) { m["Internal"] = true },
		"ipv6":     func(m map[string]any) { m["EnableIPv6"] = true },
		"icc on": func(m map[string]any) {
			m["Options"].(map[string]string)["com.docker.network.bridge.enable_icc"] = "true"
		},
		"iface":    func(m map[string]any) { m["Options"].(map[string]string)["com.docker.network.bridge.name"] = "br-123" },
		"no label": func(m map[string]any) { m["Labels"] = map[string]string{} },
		"subnet": func(m map[string]any) {
			m["IPAM"] = map[string]any{"Config": []map[string]any{{"Subnet": "10.9.0.0/24"}}}
		},
		"overlay":   func(m map[string]any) { m["Driver"] = "overlay" },
		"no config": func(m map[string]any) { m["IPAM"] = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			f, c := newFakeDaemon(t)
			body := goodEgressInspect()
			mutate(body)
			f.json("GET /networks/ah-sbx-egress", 200, body)
			_, err := c.EnsureSandboxEgressNetwork(context.Background(), "ah-sbx-egress", "10.250.0.0/24")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not a usable sandbox egress network")
			assert.False(t, f.called("POST /networks/create"))
		})
	}
}

func TestEnsureSandboxEgressNetwork_InvalidNameNeverTouchesDocker(t *testing.T) {
	f, c := newFakeDaemon(t)
	_, err := c.EnsureSandboxEgressNetwork(context.Background(), "ah-tenant-x", "10.250.0.0/24")
	require.Error(t, err)
	assert.Empty(t, f.calls)
}

func TestLookupRuntime(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /info", 200, map[string]any{"Runtimes": map[string]any{
		"runc":      map[string]any{"path": "runc"},
		"runsc":     map[string]any{"path": "/usr/local/bin/runsc"},
		"runsc-sbx": map[string]any{"path": "/usr/local/bin/runsc", "runtimeArgs": []string{"--overlay2=root:memory"}},
	}})
	rt, found, err := c.LookupRuntime(context.Background(), "runsc-sbx")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "/usr/local/bin/runsc", rt.Path)
	assert.Equal(t, []string{"--overlay2=root:memory"}, rt.Args)
	assert.True(t, IsGVisorRuntime("runsc-sbx", rt))

	_, found, err = c.LookupRuntime(context.Background(), "kata")
	require.NoError(t, err)
	assert.False(t, found)

	rt, _, _ = c.LookupRuntime(context.Background(), "runc")
	assert.False(t, IsGVisorRuntime("runc", rt))
	assert.False(t, IsGVisorRuntime("runsc-evil", RuntimeInfo{Path: "/usr/bin/crun"}), "the name alone does not make a runtime gVisor")
	assert.True(t, IsGVisorRuntime("runsc", RuntimeInfo{}))
	assert.False(t, IsGVisorRuntime("runsc-sbx", RuntimeInfo{}), "only the built-in runsc name is trusted without a path")
}

func TestEnvironmentContainerConfigs_WorkspaceTmpfs(t *testing.T) {
	cfg := RunEnvironmentConfig{TenantID: "t", EnvID: "e", Image: "img", MemoryMB: 512, VolumeName: "ah-env-e", NetworkName: "n", WorkspaceTmpfsMB: 128}
	_, h := environmentContainerConfigs(cfg, "runsc-sbx")
	require.Len(t, h.Mounts, 1)
	m := h.Mounts[0]
	assert.Equal(t, mount.TypeTmpfs, m.Type)
	assert.Equal(t, "/workspace", m.Target)
	assert.Empty(t, m.Source, "no named volume when the workspace is tmpfs")
	require.NotNil(t, m.TmpfsOptions)
	assert.Equal(t, int64(128*1024*1024), m.TmpfsOptions.SizeBytes)
	assert.Equal(t, "runsc-sbx", h.Runtime)
	_, inTmpfsMap := h.Tmpfs["/workspace"]
	assert.False(t, inTmpfsMap)

	// Default: the named volume, unchanged.
	cfg.WorkspaceTmpfsMB = 0
	_, h = environmentContainerConfigs(cfg, "runsc")
	require.Len(t, h.Mounts, 1)
	assert.Equal(t, mount.Mount{Type: mount.TypeVolume, Source: "ah-env-e", Target: "/workspace"}, h.Mounts[0])
}

func runEnvironmentCreateBody(t *testing.T, f *fakeDaemon) (container.Config, container.HostConfig) {
	t.Helper()
	var created struct {
		container.Config
		HostConfig container.HostConfig
	}
	require.NoError(t, json.Unmarshal(f.bodies["POST /containers/create"], &created))
	return created.Config, created.HostConfig
}

func TestRunEnvironment_EgressNetworkIsAttachedNotCreated(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`+"\n") })
	f.json("POST /containers/create", 201, map[string]any{"Id": "sbx-1"})
	f.on("POST /containers/sbx-1/start", noContent)

	id, err := c.RunEnvironment(context.Background(), RunEnvironmentConfig{
		TenantID: "t1", EnvID: "s1", Image: "alpine", MemoryMB: 256, CPUMillis: 500,
		NetworkName: "ah-sbx-egress", EgressNetwork: true, Runtime: "runsc-sbx", WorkspaceTmpfsMB: 64,
	})
	require.NoError(t, err)
	assert.Equal(t, "sbx-1", id)
	assert.False(t, f.called("GET /networks"), "the egress bridge is never created as an internal tenant network")
	assert.False(t, f.called("POST /networks/create"))

	_, h := runEnvironmentCreateBody(t, f)
	assert.Equal(t, "ah-sbx-egress", string(h.NetworkMode), "only the egress network")
	assert.Equal(t, "runsc-sbx", h.Runtime)
	require.Len(t, h.Mounts, 1)
	assert.Equal(t, mount.TypeTmpfs, h.Mounts[0].Type)
}

func TestRunEnvironment_DefaultsUnchanged(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`+"\n") })
	f.json("GET /networks", 200, []map[string]any{{"Name": "ah-tenant-t1", "Id": "tn"}})
	f.json("POST /containers/create", 201, map[string]any{"Id": "env-1"})
	f.on("POST /containers/env-1/start", noContent)

	_, err := c.RunEnvironment(context.Background(), RunEnvironmentConfig{
		TenantID: "t1", EnvID: "e1", Image: "alpine", MemoryMB: 256, CPUMillis: 500,
		NetworkName: "ah-tenant-t1", VolumeName: "ah-env-e1",
	})
	require.NoError(t, err)
	assert.True(t, f.called("GET /networks"), "the tenant network is ensured")
	_, h := runEnvironmentCreateBody(t, f)
	assert.Equal(t, "ah-tenant-t1", string(h.NetworkMode))
	assert.Equal(t, "runsc", h.Runtime, "the client runtime when no override is set")
	require.Len(t, h.Mounts, 1)
	assert.Equal(t, mount.TypeVolume, h.Mounts[0].Type)
	assert.Equal(t, "ah-env-e1", h.Mounts[0].Source)
}
