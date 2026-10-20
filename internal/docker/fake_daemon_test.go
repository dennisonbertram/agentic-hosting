package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDaemon is a minimal Docker Engine API stand-in. Each test registers
// handlers keyed by "METHOD /path" (API version prefix stripped); requests with
// no handler get a 404 "No such object" error, which the client maps to
// errdefs.NotFound.
type fakeDaemon struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []string
	bodies   map[string][]byte
}

func newFakeDaemon(t *testing.T) (*fakeDaemon, *DockerClient) {
	t.Helper()
	f := &fakeDaemon{t: t, handlers: map[string]http.HandlerFunc{}, bodies: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(srv.URL), client.WithVersion("1.45"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return f, &DockerClient{cli: cli}
}

func (f *fakeDaemon) on(route string, h http.HandlerFunc) { f.handlers[route] = h }

func (f *fakeDaemon) json(route string, status int, v any) {
	f.on(route, func(w http.ResponseWriter, _ *http.Request) { writeJSONResp(w, status, v) })
}

func (f *fakeDaemon) called(route string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == route {
			return true
		}
	}
	return false
}

func (f *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1.45") {
		path = strings.TrimPrefix(path, "/v1.45")
	}
	route := r.Method + " " + path
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, route)
	f.bodies[route] = body
	h := f.handlers[route]
	f.mu.Unlock()
	if h == nil {
		writeJSONResp(w, http.StatusNotFound, map[string]string{"message": "No such object: " + path})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h(w, r)
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestFakeDaemon_RunContainerCreatesHardenedServiceContainer(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.json("GET /networks", 200, []map[string]any{{"Name": "ah-ingress-t1-service-s1", "Id": "ingress-1"}})
	f.on("POST /networks/ingress-1/connect", noContent)
	f.json("POST /containers/create", 201, map[string]any{"Id": "cid-1"})
	f.on("POST /containers/cid-1/start", noContent)

	id, err := c.RunContainer(ctx, "t1", "s1", "nginx:latest", 0, map[string]string{"A": "b"}, map[string]string{"ah.tenant": "spoof"}, &ResourceLimits{MemoryMB: 256, CPUCores: 0.5})
	require.NoError(t, err)
	assert.Equal(t, "cid-1", id)

	var created struct {
		container.Config
		HostConfig container.HostConfig
	}
	require.NoError(t, json.Unmarshal(f.bodies["POST /containers/create"], &created))
	assert.Equal(t, "runsc", created.HostConfig.Runtime)
	assert.Equal(t, int64(256*1024*1024), created.HostConfig.Memory)
	assert.Equal(t, created.HostConfig.Memory, created.HostConfig.MemorySwap, "no swap")
	assert.Equal(t, int64(500_000_000), created.HostConfig.NanoCPUs)
	assert.True(t, created.HostConfig.ReadonlyRootfs)
	assert.Equal(t, []string{"ALL"}, []string(created.HostConfig.CapDrop))
	assert.Equal(t, "ah-tenant-t1", string(created.HostConfig.NetworkMode))
	assert.Equal(t, "t1", created.Labels["ah.tenant"], "control-plane labels cannot be overridden")
	assert.Equal(t, "false", created.Labels["traefik.enable"])
	assert.Contains(t, created.Env, "A=b")
	_, exposed := created.ExposedPorts["8000/tcp"]
	assert.True(t, exposed, "port defaults to 8000")
}

func TestFakeDaemon_RunContainerRemovesContainerWhenStartFails(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /networks", 200, []map[string]any{})
	f.json("POST /networks/create", 201, map[string]any{"Id": "ingress-new"})
	f.on("POST /networks/ingress-new/connect", noContent)
	f.json("POST /containers/create", 201, map[string]any{"Id": "cid-2"})
	f.json("POST /containers/cid-2/start", 500, map[string]string{"message": "runsc failed"})
	f.on("DELETE /containers/cid-2", noContent)

	_, err := c.RunContainer(context.Background(), "t1", "s1", "nginx", 8080, nil, nil, nil)
	require.Error(t, err)
	assert.True(t, f.called("DELETE /containers/cid-2"), "a container that failed to start is removed")
}

func TestFakeDaemon_RunContainerFailsWhenIngressUnavailable(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /networks", 500, map[string]string{"message": "daemon down"})
	_, err := c.RunContainer(context.Background(), "t1", "s1", "nginx", 8080, nil, nil, nil)
	require.Error(t, err)
	assert.False(t, f.called("POST /containers/create"))
}

func TestFakeDaemon_EnsureNetworkHandlesCreateRace(t *testing.T) {
	f, c := newFakeDaemon(t)
	listed := 0
	f.on("GET /networks", func(w http.ResponseWriter, _ *http.Request) {
		listed++
		if listed == 1 {
			writeJSONResp(w, 200, []map[string]any{})
			return
		}
		writeJSONResp(w, 200, []map[string]any{{"Name": "ah-tenant-t1", "Id": "raced"}})
	})
	f.json("POST /networks/create", 409, map[string]string{"message": "network with name ah-tenant-t1 already exists"})

	id, err := c.EnsureNetwork(context.Background(), "ah-tenant-t1")
	require.NoError(t, err)
	assert.Equal(t, "raced", id)

	var opts map[string]any
	require.NoError(t, json.Unmarshal(f.bodies["POST /networks/create"], &opts))
	assert.Equal(t, true, opts["Internal"])
}

func TestFakeDaemon_ConnectAndDisconnectAreIdempotent(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.json("POST /networks/n1/connect", 403, map[string]string{"message": "endpoint with name x already exists in network n1"})
	f.json("POST /networks/n1/disconnect", 403, map[string]string{"message": "container x is not connected to network n1"})
	f.json("POST /networks/n2/connect", 500, map[string]string{"message": "boom"})

	assert.NoError(t, c.ConnectNetwork(ctx, "n1", "x"))
	assert.NoError(t, c.ConnectNetworkAlias(ctx, "n1", "x", "alias"))
	assert.NoError(t, c.NetworkDisconnect(ctx, "n1", "x"))
	assert.Error(t, c.ConnectNetwork(ctx, "n2", "x"))
	assert.Error(t, c.ConnectNetworkAlias(ctx, "n2", "x", "alias"))
}

func TestFakeDaemon_NetworkListAndInspect(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /networks", 200, []map[string]any{{"Name": "a", "Id": "1", "Containers": map[string]any{"c1": map[string]string{"Name": "svc"}}}})
	f.json("GET /networks/1", 200, map[string]any{"Name": "a", "Id": "1", "Containers": map[string]any{"c1": map[string]string{"Name": "svc"}}})
	f.on("DELETE /networks/1", noContent)

	nets, err := c.NetworkList(context.Background())
	require.NoError(t, err)
	require.Len(t, nets, 1)
	assert.Equal(t, 1, nets[0].Containers)
	assert.Equal(t, []string{"svc"}, nets[0].ContainerNames)

	info, err := c.NetworkInspect(context.Background(), "1")
	require.NoError(t, err)
	assert.Equal(t, []string{"svc"}, info.ContainerNames)
	assert.NoError(t, c.RemoveNetwork(context.Background(), "1"))

	_, err = c.NetworkInspect(context.Background(), "missing")
	assert.True(t, errdefs.IsNotFound(err))
}

func TestFakeDaemon_InspectContainer(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /containers/cid/json", 200, map[string]any{
		"Id": "cid", "Name": "/ah-t-s", "Created": "2026-01-02T03:04:05Z", "RestartCount": 2,
		"State":  map[string]any{"Status": "Running", "ExitCode": 0, "StartedAt": "x", "Health": map[string]any{"Status": "healthy"}},
		"Config": map[string]any{"Env": []string{"PORT=9090"}, "Labels": map[string]string{"ah.tenant": "t"}},
	})

	info, err := c.InspectContainer(context.Background(), "cid")
	require.NoError(t, err)
	assert.Equal(t, "running", info.Status)
	assert.Equal(t, "healthy", info.HealthStatus)
	assert.Equal(t, 9090, info.EffectivePort)
	assert.Equal(t, 2, info.RestartCount)
	assert.Equal(t, 2026, info.CreatedAt.Year())
	assert.Equal(t, map[string]string{"ah.tenant": "t"}, c.GetContainerLabels(context.Background(), "cid"))
	assert.Equal(t, "/ah-t-s", c.GetContainerName(context.Background(), "cid"))

	_, err = c.InspectContainer(context.Background(), "gone")
	require.Error(t, err)
	assert.True(t, errdefs.IsNotFound(err), "not-found must survive wrapping so callers can self-heal")
	assert.Nil(t, c.GetContainerLabels(context.Background(), "gone"))
	assert.Empty(t, c.GetContainerName(context.Background(), "gone"))
}

func TestFakeDaemon_ContainerLifecycle(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.on("POST /containers/cid/stop", noContent)
	f.on("POST /containers/cid/start", noContent)
	f.on("POST /containers/cid/restart", noContent)
	f.on("DELETE /containers/cid", noContent)
	f.on("POST /containers/cid/rename", noContent)
	f.on("GET /containers/cid/logs", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "50", r.URL.Query().Get("tail"))
		_, _ = io.WriteString(w, "log")
	})

	assert.NoError(t, c.StopContainer(ctx, "cid"))
	assert.NoError(t, c.StartContainer(ctx, "cid"))
	assert.NoError(t, c.RestartContainer(ctx, "cid"))
	assert.NoError(t, c.RemoveContainer(ctx, "cid"))
	assert.NoError(t, c.RenameContainer(ctx, "cid", "new"))
	rc, err := c.LogsContainer(ctx, "cid", false, 50)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, errdefs.IsNotFound(c.StopContainer(ctx, "missing")))
}

func TestFakeDaemon_StopAndRemoveByName(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /containers/ah-t-s/json", 200, map[string]any{"Id": "real-id", "State": map[string]any{}, "Config": map[string]any{}})
	f.on("POST /containers/real-id/stop", noContent)
	f.on("DELETE /containers/real-id", noContent)

	require.NoError(t, c.StopAndRemoveByName(context.Background(), "ah-t-s"))
	assert.True(t, f.called("DELETE /containers/real-id"), "removal uses the resolved ID")
	assert.True(t, errdefs.IsNotFound(c.StopAndRemoveByName(context.Background(), "ah-x-y")))
}

func TestFakeDaemon_ListContainers(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /containers/json", 200, []map[string]any{
		{"Id": "a", "Names": []string{"/one"}, "Image": "img-a", "ImageID": "sha-a", "Labels": map[string]string{"ah.tenant": "t1"}},
		{"Id": "b", "Names": []string{"/two"}, "Image": "img-b", "ImageID": "sha-b", "Labels": map[string]string{"ah.tenant": "t2"}},
		{"Id": "c", "Names": []string{"/three"}, "Labels": map[string]string{}},
	})
	ctx := context.Background()

	ids, err := c.ListContainersByLabel(ctx, "ah.tenant", "t1")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, ids)
	ids, err = c.ListContainersByLabel(ctx, "ah.tenant", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, ids, "empty value matches any value of the label")

	imgs, err := c.ListContainerImages(ctx)
	require.NoError(t, err)
	assert.Contains(t, imgs, "img-a")
	assert.Contains(t, imgs, "sha-b")

	names, err := c.ListNetworkContainers(ctx, "net")
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two", "three"}, names)
}

func TestFakeDaemon_Images(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.json("GET /images/present:1/json", 200, map[string]any{"Id": "sha256:x"})
	f.json("GET /images/broken:1/json", 500, map[string]string{"message": "boom"})
	f.on("POST /images/src:1/tag", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	f.json("DELETE /images/old:1", 200, []map[string]string{{"Untagged": "old:1"}})
	f.json("POST /images/prune", 200, map[string]any{"ImagesDeleted": []map[string]string{{"Deleted": "a"}, {"Deleted": "b"}}})
	f.json("GET /images/json", 200, []map[string]any{{"Id": "i1", "RepoTags": []string{"x:1"}, "Created": 100}})

	ok, err := c.ImageExists(ctx, "present:1")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = c.ImageExists(ctx, "absent:1")
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = c.ImageExists(ctx, "broken:1")
	assert.Error(t, err)

	assert.NoError(t, c.TagImage(ctx, "src:1", "dst:1"))
	assert.NoError(t, c.RemoveImage(ctx, "old:1"))
	n, err := c.PruneDanglingImages(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	imgs, err := c.ListImages(ctx)
	require.NoError(t, err)
	require.Len(t, imgs, 1)
	assert.Equal(t, []string{"x:1"}, imgs[0].Tags)
	assert.Equal(t, int64(100), imgs[0].Created.Unix())
}

func TestFakeDaemon_PullImage(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.on("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fromImage") == "bad" {
			_, _ = io.WriteString(w, `{"error":"denied"}`+"\n")
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`+"\n")
	})
	assert.NoError(t, c.PullImage(context.Background(), "good:1"))
	assert.Error(t, c.PullImage(context.Background(), "bad:1"), "an error inside a 200 stream fails the pull")
}

func TestFakeDaemon_VolumesAndInfo(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.json("POST /volumes/create", 201, map[string]any{"Name": "v1"})
	f.on("DELETE /volumes/v1", noContent)
	f.json("GET /volumes", 200, map[string]any{"Volumes": []map[string]any{{"Name": "ah-db-1"}, {"Name": "ah-db-2"}}})
	f.json("POST /volumes/prune", 200, map[string]any{"VolumesDeleted": []string{"x"}})
	f.json("GET /info", 200, map[string]any{"Runtimes": map[string]any{"runsc": map[string]any{"path": "/usr/bin/runsc"}}})
	f.json("GET /system/df", 200, map[string]any{
		"Images":     []map[string]any{{"Size": 10}},
		"Containers": []map[string]any{{"SizeRw": 5}},
		"Volumes":    []map[string]any{{"UsageData": map[string]any{"Size": 7}}},
		"BuildCache": []map[string]any{{"Size": 3}},
	})

	assert.NoError(t, c.CreateVolume(ctx, "v1"))
	assert.NoError(t, c.RemoveVolume(ctx, "v1"))
	assert.NoError(t, c.RemoveVolumeSafe(ctx, "v1"))
	names, err := c.ListVolumes(ctx, "ah-db-")
	require.NoError(t, err)
	assert.Equal(t, []string{"ah-db-1", "ah-db-2"}, names)
	n, err := c.PruneAnonymousVolumes(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoError(t, c.VerifyGVisorRuntime(ctx))
	du, err := c.DiskUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, &StorageUsage{ImagesSize: 10, ContainersSize: 5, VolumesSize: 7, BuildCacheSize: 3}, du)

	f.json("GET /info", 200, map[string]any{"Runtimes": map[string]any{"runc": map[string]any{}}})
	assert.Error(t, c.VerifyGVisorRuntime(ctx), "missing runsc is fatal")

	f.json("GET /info", 200, map[string]any{"Runtimes": map[string]any{"runsc": map[string]any{"runtimeType": "io.containerd.runc.v2"}}})
	assert.Error(t, c.VerifyGVisorRuntime(ctx), "runc registered under the runsc name is fatal")

	f.json("GET /info", 200, map[string]any{"Runtimes": map[string]any{"runsc": map[string]any{"runtimeType": "io.containerd.runsc.v1"}}})
	assert.NoError(t, c.VerifyGVisorRuntime(ctx), "gVisor shim runtime is accepted")
}

func TestFakeDaemon_RunDatabaseAndEnvironment(t *testing.T) {
	f, c := newFakeDaemon(t)
	ctx := context.Background()
	f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`+"\n") })
	f.json("POST /containers/create", 201, map[string]any{"Id": "db-1"})
	f.on("POST /containers/db-1/start", noContent)
	f.json("GET /networks", 200, []map[string]any{{"Name": "ah-tenant-t1", "Id": "tn"}})

	id, err := c.RunDatabase(ctx, RunDatabaseConfig{Name: "ah-db", Image: "postgres:16", HostPort: 15432, ContainerPort: 5432,
		Env: map[string]string{"P": "x"}, Cmd: []string{"postgres"}, VolumeName: "vol", MountPath: "/data",
		NetworkName: "ah-dbnet-t1-d1", NetworkAlias: "ah-db-d1"})
	require.NoError(t, err)
	assert.Equal(t, "db-1", id)
	var created struct {
		container.Config
		HostConfig       container.HostConfig
		NetworkingConfig struct {
			EndpointsConfig map[string]struct{ Aliases []string }
		}
	}
	require.NoError(t, json.Unmarshal(f.bodies["POST /containers/create"], &created))
	assert.Equal(t, "127.0.0.1", created.HostConfig.PortBindings["5432/tcp"][0].HostIP)
	assert.Equal(t, []string{"ah-db-d1"}, created.NetworkingConfig.EndpointsConfig["ah-dbnet-t1-d1"].Aliases)
	assert.Equal(t, "database", created.Labels["ah.type"])

	id, err = c.RunEnvironment(ctx, RunEnvironmentConfig{TenantID: "t1", EnvID: "e1", Image: "ubuntu", MemoryMB: 128, CPUMillis: 500, VolumeName: "ws", NetworkName: "ah-tenant-t1"})
	require.NoError(t, err)
	assert.Equal(t, "db-1", id)
	require.NoError(t, json.Unmarshal(f.bodies["POST /containers/create"], &created))
	assert.Equal(t, "environment", created.Labels["ah.type"])
	assert.Equal(t, int64(500_000_000), created.HostConfig.NanoCPUs)
}

func TestFakeDaemon_WipeVolume(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /images/busybox:1.36/json", 200, map[string]any{"Id": "bb"})
	f.json("POST /containers/create", 201, map[string]any{"Id": "wipe-1"})
	f.on("POST /containers/wipe-1/start", noContent)
	f.json("POST /containers/wipe-1/wait", 200, map[string]any{"StatusCode": 0})
	f.on("DELETE /containers/wipe-1", noContent)
	require.NoError(t, c.WipeVolume(context.Background(), "vol"))
	assert.True(t, f.called("DELETE /containers/wipe-1"), "wipe container is always force-removed")

	f.json("POST /containers/wipe-1/wait", 200, map[string]any{"StatusCode": 1})
	assert.Error(t, c.WipeVolume(context.Background(), "vol"), "non-zero exit fails the wipe")
}

func TestFakeDaemon_ContainerStatsAndExecCreate(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.json("GET /containers/cid/stats", 200, map[string]any{
		"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 42}},
		"memory_stats": map[string]any{"usage": 1024},
	})
	f.json("POST /containers/cid/exec", 201, map[string]any{"Id": "exec-1"})

	stats, err := c.ContainerStats(context.Background(), "cid")
	require.NoError(t, err)
	assert.Equal(t, &ContainerStats{CPUTotalNanos: 42, MemoryBytes: 1024}, stats)

	id, err := c.ExecCreate(context.Background(), "cid", []string{"ls"}, "")
	require.NoError(t, err)
	assert.Equal(t, "exec-1", id)
	var opts container.ExecOptions
	require.NoError(t, json.Unmarshal(f.bodies["POST /containers/cid/exec"], &opts))
	assert.Equal(t, "/workspace", opts.WorkingDir)
}

// TestCappedWriter_DoesNotBreakStdCopy reproduces exec output above the cap:
// a short write count would make stdcopy.StdCopy fail with io.ErrShortWrite.
func TestCappedWriter_DoesNotBreakStdCopy(t *testing.T) {
	var stream bytes.Buffer
	out := stdcopy.NewStdWriter(&stream, stdcopy.Stdout)
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	for i := 0; i < 4; i++ {
		_, err := out.Write(chunk)
		require.NoError(t, err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	_, err := stdcopy.StdCopy(&cappedWriter{buf: &stdoutBuf, max: 100 * 1024}, &cappedWriter{buf: &stderrBuf, max: 10}, &stream)
	require.NoError(t, err)
	assert.Equal(t, 100*1024, stdoutBuf.Len())
}

func TestFakeDaemon_RespectsContextDeadline(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.on("POST /containers/slow/stop", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.Error(t, c.StopContainer(ctx, "slow"))
}
