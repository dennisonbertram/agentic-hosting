package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const otherTenantID = "t_other_tenant_002"

type fakeSnapshots struct {
	images map[string]string // "tenant/id" -> image
	env    map[string]map[string]string
	envErr error
}

func (f *fakeSnapshots) SnapshotImage(_ context.Context, tenantID, id string) (string, error) {
	img, ok := f.images[tenantID+"/"+id]
	if !ok {
		return "", apierr.NotFound("snapshot not found")
	}
	return img, nil
}

func (f *fakeSnapshots) SnapshotEnv(_ context.Context, tenantID, id string) (map[string]string, error) {
	if f.envErr != nil {
		return nil, f.envErr
	}
	if _, ok := f.images[tenantID+"/"+id]; !ok {
		return nil, apierr.NotFound("snapshot not found")
	}
	return f.env[tenantID+"/"+id], nil
}

type fakeServices struct {
	images map[string]string
	env    map[string]map[string]string
	calls  []bool // withEnv per call
}

func (f *fakeServices) ServiceImageAndEnv(_ context.Context, tenantID, id string, withEnv bool) (string, map[string]string, error) {
	f.calls = append(f.calls, withEnv)
	img, ok := f.images[tenantID+"/"+id]
	if !ok {
		return "", nil, apierr.NotFound("service not found")
	}
	if !withEnv {
		return img, nil, nil
	}
	return img, f.env[tenantID+"/"+id], nil
}

func newSandboxManager(t *testing.T) (*Manager, *testutil.MockDockerClient, *fakeSnapshots, *fakeServices) {
	t.Helper()
	mgr, mock := newTestManager(t, 5)
	seedTenant(t, mgr.db, otherTenantID, 5)
	snaps := &fakeSnapshots{
		images: map[string]string{
			testTenantID + "/snap1":  "127.0.0.1:5000/snapshots/" + testTenantID + ":snap1",
			otherTenantID + "/snap2": "127.0.0.1:5000/snapshots/" + otherTenantID + ":snap2",
			testTenantID + "/evil":   "127.0.0.1:5000/snapshots/" + otherTenantID + ":snap2",
		},
		env: map[string]map[string]string{testTenantID + "/snap1": {"SECRET": "s3cr3t", "A": "1"}},
	}
	svcs := &fakeServices{
		images: map[string]string{
			testTenantID + "/svc1":   "127.0.0.1:5000/ah/" + testTenantID + "-svc1:b1",
			testTenantID + "/nobld":  "",
			otherTenantID + "/svc2":  "127.0.0.1:5000/ah/" + otherTenantID + "-svc2:b1",
			testTenantID + "/hubsvc": "nginx:1.27",
		},
		env: map[string]map[string]string{testTenantID + "/svc1": {"DATABASE_URL": "postgres://x"}},
	}
	mgr.SetSandboxSources(snaps, svcs)
	mgr.diskCheck = func() error { return nil }
	return mgr, mock, snaps, svcs
}

func intp(v int) *int { return &v }

func lastRunCfg(t *testing.T, mock *testutil.MockDockerClient) docker.RunEnvironmentConfig {
	t.Helper()
	require.NotEmpty(t, mock.RunEnvironmentConfigs)
	return mock.RunEnvironmentConfigs[len(mock.RunEnvironmentConfigs)-1]
}

func TestCreateSandbox_Template(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "template", Template: "default"},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(s.Name, "sbx-"))
	assert.Equal(t, "running", s.Status)
	assert.Equal(t, "idle", s.Run)
	assert.Equal(t, "none", s.Egress)
	assert.Equal(t, SandboxSourceRef{Type: "template", Ref: "default"}, s.Source)
	assert.Equal(t, "ubuntu:24.04", s.Image)
	assert.Equal(t, 1800, s.TTLSeconds)
	assert.Equal(t, s.CreatedAt+1800, s.ExpiresAt)
	assert.Equal(t, s.CreatedAt+86400, s.MaxExpiresAt)

	cfg := lastRunCfg(t, mock)
	assert.Equal(t, "ubuntu:24.04", cfg.Image)
	assert.False(t, cfg.UseImageCmd)
	assert.False(t, cfg.LocalImage)
	assert.Empty(t, cfg.Env)
	assert.Equal(t, docker.TenantNetworkName(testTenantID), cfg.NetworkName)

	var kind string
	require.NoError(t, mgr.db.QueryRow(`SELECT kind FROM environments WHERE id = ?`, s.ID).Scan(&kind))
	assert.Equal(t, KindSandbox, kind)

	got, err := mgr.GetSandbox(context.Background(), testTenantID, s.ID)
	require.NoError(t, err)
	assert.Equal(t, s.ExpiresAt, got.ExpiresAt)
	assert.Equal(t, s.MaxExpiresAt, got.MaxExpiresAt)
	assert.Equal(t, 512, got.MemoryMB)
	assert.Equal(t, 1000, got.CPUMillicores)
}

func TestCreateSandbox_Image(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Name: "py", Source: SandboxSource{Type: "image", Image: "python:3.12-slim"},
		MemoryMB: intp(256), CPUMillicores: intp(500),
	})
	require.NoError(t, err)
	assert.Equal(t, "py", s.Name)
	cfg := lastRunCfg(t, mock)
	assert.Equal(t, "python:3.12-slim", cfg.Image)
	assert.Equal(t, int64(256), cfg.MemoryMB)
	assert.Equal(t, int64(500), cfg.CPUMillis)
	assert.False(t, cfg.UseImageCmd)
	assert.False(t, cfg.LocalImage)
}

func TestCreateSandbox_SnapshotWithAndWithoutEnv(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "snapshot", SnapshotID: "snap1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "app", s.Run)
	cfg := lastRunCfg(t, mock)
	assert.True(t, cfg.UseImageCmd)
	assert.True(t, cfg.LocalImage)
	assert.Empty(t, cfg.Env, "include_env=false passes no env")

	s2, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "snapshot", SnapshotID: "snap1"}, IncludeEnv: true, Run: "idle",
	})
	require.NoError(t, err)
	cfg = lastRunCfg(t, mock)
	assert.Equal(t, []string{"A=1", "SECRET=s3cr3t"}, cfg.Env)
	assert.False(t, cfg.UseImageCmd, "run=idle overrides the app default")
	b, err := json.Marshal(s2)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "s3cr3t")
	assert.NotContains(t, string(b), "container")
	assert.NotContains(t, string(b), "volume")
	got, err := mgr.GetSandbox(context.Background(), testTenantID, s2.ID)
	require.NoError(t, err)
	b, _ = json.Marshal(got)
	assert.NotContains(t, string(b), "s3cr3t")
}

func TestCreateSandbox_Service(t *testing.T) {
	mgr, mock, _, svcs := newSandboxManager(t)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "service", ServiceID: "svc1"}, IncludeEnv: true,
	})
	require.NoError(t, err)
	cfg := lastRunCfg(t, mock)
	assert.Equal(t, "127.0.0.1:5000/ah/"+testTenantID+"-svc1:b1", cfg.Image)
	assert.True(t, cfg.UseImageCmd)
	assert.True(t, cfg.LocalImage)
	assert.Equal(t, []string{"DATABASE_URL=postgres://x"}, cfg.Env)
	assert.Equal(t, []bool{true}, svcs.calls)

	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "service", ServiceID: "nobld"},
	})
	assertAPICode(t, err, 409)
}

func TestCreateSandbox_CrossTenantAndImageRules(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	cases := []struct {
		name string
		src  SandboxSource
		code int
	}{
		{"other tenant snapshot", SandboxSource{Type: "snapshot", SnapshotID: "snap2"}, 404},
		{"other tenant service", SandboxSource{Type: "service", ServiceID: "svc2"}, 404},
		{"snapshot pointing at other tenant image", SandboxSource{Type: "snapshot", SnapshotID: "evil"}, 400},
		{"other tenant local image", SandboxSource{Type: "image", Image: "127.0.0.1:5000/ah/" + otherTenantID + "-x:1"}, 400},
		{"localhost registry", SandboxSource{Type: "image", Image: "localhost/x"}, 400},
		{"custom registry", SandboxSource{Type: "image", Image: "ghcr.io/x"}, 400},
		{"empty image", SandboxSource{Type: "image"}, 400},
		{"unknown template", SandboxSource{Type: "template", Template: "nope"}, 404},
		{"no type", SandboxSource{}, 400},
		{"bad type", SandboxSource{Type: "git"}, 400},
		{"snapshot without id", SandboxSource{Type: "snapshot"}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tc.src})
			assertAPICode(t, err, tc.code)
		})
	}
	assert.Zero(t, mock.RunEnvironmentCalls)

	// The tenant's own local image is allowed and pulled with the local fallback.
	_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "image", Image: "127.0.0.1:5000/ah/" + testTenantID + "-x:1"},
	})
	require.NoError(t, err)
	assert.True(t, lastRunCfg(t, mock).LocalImage)
}

func TestCreateSandbox_Validation(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	tmpl := SandboxSource{Type: "template"}
	cases := []struct {
		name string
		req  SandboxCreateRequest
		code int
	}{
		{"egress internet", SandboxCreateRequest{Source: tmpl, Egress: "internet"}, 403},
		{"egress unknown", SandboxCreateRequest{Source: tmpl, Egress: "lan"}, 400},
		{"ttl too small", SandboxCreateRequest{Source: tmpl, TTLSeconds: intp(59)}, 400},
		{"ttl over quota", SandboxCreateRequest{Source: tmpl, TTLSeconds: intp(14401)}, 400},
		{"memory too small", SandboxCreateRequest{Source: tmpl, MemoryMB: intp(127)}, 400},
		{"memory over cap", SandboxCreateRequest{Source: tmpl, MemoryMB: intp(2049)}, 400},
		{"cpu too small", SandboxCreateRequest{Source: tmpl, CPUMillicores: intp(99)}, 400},
		{"cpu over cap", SandboxCreateRequest{Source: tmpl, CPUMillicores: intp(2001)}, 400},
		{"bad run", SandboxCreateRequest{Source: tmpl, Run: "daemon"}, 400},
		{"bad name", SandboxCreateRequest{Source: tmpl, Name: "1abc"}, 400},
		{"include_env on template", SandboxCreateRequest{Source: tmpl, IncludeEnv: true}, 400},
		{"include_env on image", SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "alpine"}, IncludeEnv: true}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.CreateSandbox(ctx, testTenantID, tc.req)
			assertAPICode(t, err, tc.code)
		})
	}
	assert.Zero(t, mock.RunEnvironmentCalls)

	// Bounds are inclusive.
	_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tmpl, TTLSeconds: intp(60)})
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tmpl, TTLSeconds: intp(14400),
		MemoryMB: intp(2048), CPUMillicores: intp(2000)})
	require.NoError(t, err)

	// A tighter tenant quota tightens the bounds.
	_, err = mgr.db.Exec(`UPDATE tenant_quotas SET max_memory_mb = 256, max_cpu_cores = 0.5, max_sandbox_ttl_seconds = 600 WHERE tenant_id = ?`, testTenantID)
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tmpl, MemoryMB: intp(512)})
	assertAPICode(t, err, 400)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tmpl, CPUMillicores: intp(600)})
	assertAPICode(t, err, 400)
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: tmpl})
	require.NoError(t, err)
	assert.Equal(t, 256, s.MemoryMB, "template default is clamped to the tenant quota")
	assert.Equal(t, 500, s.CPUMillicores)
	assert.Equal(t, 600, s.TTLSeconds, "default TTL is clamped to the tenant max")
}

func TestCreateSandbox_DiskCheckRefuses(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.diskCheck = func() error { return errors.New("disk full") }
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	assertAPICode(t, err, 503)
	assert.Zero(t, mock.RunEnvironmentCalls)
}

func TestCreateSandbox_NoSourcesWired(t *testing.T) {
	mgr, _ := newTestManager(t, 5)
	mgr.diskCheck = func() error { return nil }
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "snapshot", SnapshotID: "x"}})
	assertAPICode(t, err, 503)
	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "service", ServiceID: "x"}})
	assertAPICode(t, err, 503)
}

func TestCreateSandbox_OnlyTemplateUsesWarmPool(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	pool := NewPoolManager(mgr.db, mock, PoolConfig{Enabled: true, PoolSize: 2, MaxTotal: 6})
	mgr.SetPool(pool)
	for i := 0; i < 2; i++ {
		_, err := mgr.db.Exec(`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
			VALUES (?, 'tmpl_default', ?, ?, 'ready', ?)`, fmt.Sprintf("wp%d", i), fmt.Sprintf("warm-%d", i), fmt.Sprintf("ah-pool-wp%d", i), time.Now().Unix())
		require.NoError(t, err)
	}
	ctx := context.Background()

	_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "ubuntu:24.04"}})
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "snapshot", SnapshotID: "snap1"}})
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}, MemoryMB: intp(256)})
	require.NoError(t, err, "custom resources skip the pool")
	assert.Equal(t, 3, mock.RunEnvironmentCalls)
	var ready int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&ready))
	assert.Equal(t, 2, ready)

	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	assert.Equal(t, 3, mock.RunEnvironmentCalls, "template sandbox took a warm container")
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&ready))
	assert.Equal(t, 1, ready)
}

func TestCreateSandbox_RunFailureRollsBack(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) {
		return "", errors.New("pull failed")
	}
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.Error(t, err)
	var status, vol string
	require.NoError(t, mgr.db.QueryRow(`SELECT status, volume_name FROM environments WHERE kind = 'sandbox'`).Scan(&status, &vol))
	assert.Equal(t, "failed", status)
	assert.Contains(t, mock.RemoveVolumeCalls, vol)
}

func TestSandboxQuota_PerTenantGlobalAndSeparateFromEnvironments(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	ctx := context.Background()
	_, err := mgr.db.Exec(`UPDATE tenant_quotas SET max_sandboxes = 2, max_environments = 1`)
	require.NoError(t, err)

	// Environments and sandboxes are counted separately.
	_, err = mgr.Create(ctx, testTenantID, CreateRequest{Name: "dev"})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
		require.NoError(t, err)
	}
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.ErrorIs(t, err, apierr.ErrQuotaExceeded)
	assert.Contains(t, err.Error(), "maximum 2 sandboxes")

	// Host-wide cap: the other tenant has its own quota left but the host is full.
	mgr.SetMaxSandboxesTotal(3)
	_, err = mgr.CreateSandbox(ctx, otherTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, otherTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.ErrorIs(t, err, apierr.ErrQuotaExceeded)
	assert.Contains(t, err.Error(), "host sandbox capacity")

	// Environments are not affected by the sandbox host cap.
	_, err = mgr.Create(ctx, otherTenantID, CreateRequest{Name: "dev2"})
	require.NoError(t, err)
}

func TestSandboxQuota_ConcurrentCreates(t *testing.T) {
	db := testutil.NewFileStateDB(t)
	seedTenant(t, db, testTenantID, 5)
	_, err := db.Exec(`UPDATE tenant_quotas SET max_sandboxes = 3`)
	require.NoError(t, err)
	mgr := NewManager(db, &testutil.MockDockerClient{}, "", "")
	mgr.diskCheck = func() error { return nil }

	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.CreateSandbox(context.Background(), testTenantID,
				SandboxCreateRequest{Source: SandboxSource{Type: "template"}}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else {
				assert.ErrorIs(t, err, apierr.ErrQuotaExceeded)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 3, ok)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM environments WHERE kind = 'sandbox' AND status != 'failed'`).Scan(&count))
	assert.Equal(t, 3, count)
}

func TestSandbox_KindScoping(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	ctx := context.Background()
	env, err := mgr.Create(ctx, testTenantID, CreateRequest{Name: "dev"})
	require.NoError(t, err)
	sbx, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)

	// Environment methods do not see sandboxes.
	_, err = mgr.Get(ctx, testTenantID, sbx.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	require.ErrorIs(t, mgr.Delete(ctx, testTenantID, sbx.ID), apierr.ErrNotFound)
	_, err = mgr.Exec(ctx, testTenantID, sbx.ID, ExecRequest{Command: []string{"ls"}})
	require.ErrorIs(t, err, apierr.ErrNotFound)
	require.ErrorIs(t, mgr.ExtendLease(ctx, testTenantID, sbx.ID, 600), apierr.ErrNotFound)
	require.ErrorIs(t, mgr.Stop(ctx, testTenantID, sbx.ID), apierr.ErrNotFound)
	envs, err := mgr.List(ctx, testTenantID, 50, 0)
	require.NoError(t, err)
	require.Len(t, envs, 1)
	assert.Equal(t, env.ID, envs[0].ID)

	// Sandbox methods do not see environments.
	_, err = mgr.GetSandbox(ctx, testTenantID, env.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	require.ErrorIs(t, mgr.DeleteSandbox(ctx, testTenantID, env.ID), apierr.ErrNotFound)
	_, err = mgr.ExecSandbox(ctx, testTenantID, env.ID, ExecRequest{Command: []string{"ls"}})
	require.ErrorIs(t, err, apierr.ErrNotFound)
	_, err = mgr.ExtendSandbox(ctx, testTenantID, env.ID, 600)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	sbxs, err := mgr.ListSandboxes(ctx, testTenantID, 50, 0, "")
	require.NoError(t, err)
	require.Len(t, sbxs, 1)
	assert.Equal(t, sbx.ID, sbxs[0].ID)

	// Other tenants see neither.
	_, err = mgr.GetSandbox(ctx, otherTenantID, sbx.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	require.ErrorIs(t, mgr.DeleteSandbox(ctx, otherTenantID, sbx.ID), apierr.ErrNotFound)

	// Both rows still exist.
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM environments`).Scan(&n))
	assert.Equal(t, 2, n)
}

func TestListSandboxes_StatusFilterAndPaging(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
		require.NoError(t, err)
		ids = append(ids, s.ID)
	}
	_, err := mgr.db.Exec(`UPDATE environments SET status = 'stopped' WHERE id = ?`, ids[0])
	require.NoError(t, err)
	got, err := mgr.ListSandboxes(ctx, testTenantID, 50, 0, "stopped")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ids[0], got[0].ID)
	got, err = mgr.ListSandboxes(ctx, testTenantID, 2, 0, "")
	require.NoError(t, err)
	assert.Len(t, got, 2)
	got, err = mgr.ListSandboxes(ctx, testTenantID, 2, 2, "")
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestExecSandbox_BumpsActivityAndUsesTimeout(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	_, err = mgr.db.Exec(`UPDATE environments SET last_activity_at = 1 WHERE id = ?`, s.ID)
	require.NoError(t, err)
	var gotCmd []string
	mock.ExecCreateFn = func(_ context.Context, _ string, cmd []string, _ string) (string, error) {
		gotCmd = cmd
		return "exec-1", nil
	}
	mock.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		return []byte("hi\n"), nil, 3, nil
	}
	resp, err := mgr.ExecSandbox(ctx, testTenantID, s.ID, ExecRequest{Command: []string{"echo", "hi"}, Timeout: 10})
	require.NoError(t, err)
	assert.Equal(t, 3, resp.ExitCode)
	assert.Equal(t, "hi\n", resp.Stdout)
	assert.Equal(t, []string{"timeout", "-s", "KILL", "15", "echo", "hi"}, gotCmd)
	got, err := mgr.GetSandbox(ctx, testTenantID, s.ID)
	require.NoError(t, err)
	assert.Greater(t, got.LastActivityAt, int64(1))

	_, err = mgr.db.Exec(`UPDATE environments SET status = 'stopped' WHERE id = ?`, s.ID)
	require.NoError(t, err)
	_, err = mgr.ExecSandbox(ctx, testTenantID, s.ID, ExecRequest{Command: []string{"ls"}})
	assertAPICode(t, err, 400)
}

func TestDeleteSandbox_RemovesEverything(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	require.NoError(t, mgr.DeleteSandbox(ctx, testTenantID, s.ID))
	assert.Contains(t, mock.StopContainerCalls, "mock-env-container-"+s.ID)
	assert.Contains(t, mock.RemoveContainerCalls, "mock-env-container-"+s.ID)
	assert.Contains(t, mock.RemoveVolumeCalls, "ah-env-"+s.ID)
	_, err = mgr.GetSandbox(ctx, testTenantID, s.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	require.ErrorIs(t, mgr.DeleteSandbox(ctx, testTenantID, s.ID), apierr.ErrNotFound)
}

func TestDeleteSandbox_DockerFailureKeepsRowForRetry(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("daemon busy") }
	require.Error(t, mgr.DeleteSandbox(ctx, testTenantID, s.ID))
	got, err := mgr.GetSandbox(ctx, testTenantID, s.ID)
	require.NoError(t, err)
	assert.Equal(t, "deleting", got.Status)
}

func TestDeleteSandbox_DuringCreateWins(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	started := make(chan string)
	release := make(chan struct{})
	mock.RunEnvironmentFn = func(_ context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		started <- cfg.EnvID
		<-release
		return "late-container", nil
	}
	errc := make(chan error, 1)
	go func() {
		_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
		errc <- err
	}()
	id := <-started
	require.NoError(t, mgr.DeleteSandbox(ctx, testTenantID, id))
	close(release)
	err := <-errc
	assertAPICode(t, err, 409)
	assert.Contains(t, mock.RemoveContainerCalls, "late-container", "create removes the container it made")
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM environments WHERE id = ?`, id).Scan(&n))
	assert.Zero(t, n)
}

func TestExtendSandbox(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	ctx := context.Background()
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}, TTLSeconds: intp(120)})
	require.NoError(t, err)

	got, err := mgr.ExtendSandbox(ctx, testTenantID, s.ID, 3600)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, got.ExpiresAt, time.Now().Unix()+3599)
	assert.Equal(t, 3600, got.TTLSeconds)
	assert.Equal(t, s.MaxExpiresAt, got.MaxExpiresAt)

	_, err = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 59)
	assertAPICode(t, err, 400)
	_, err = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 14401)
	assertAPICode(t, err, 400)
	_, err = mgr.ExtendSandbox(ctx, otherTenantID, s.ID, 600)
	require.ErrorIs(t, err, apierr.ErrNotFound)

	// Past max_expires_at.
	_, err = mgr.db.Exec(`UPDATE environments SET max_expires_at = ? WHERE id = ?`, time.Now().Unix()+300, s.ID)
	require.NoError(t, err)
	_, err = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 600)
	assertAPICode(t, err, 400)
	assert.Contains(t, err.Error(), "maximum lifetime")
	_, err = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 120)
	require.NoError(t, err)

	// Not running.
	_, err = mgr.db.Exec(`UPDATE environments SET status = 'stopped' WHERE id = ?`, s.ID)
	require.NoError(t, err)
	_, err = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 120)
	assertAPICode(t, err, 409)
}

func TestExtendSandbox_RaceWithReaperHasOneWinner(t *testing.T) {
	for i := 0; i < 20; i++ {
		db := testutil.NewFileStateDB(t)
		seedTenant(t, db, testTenantID, 5)
		mgr := NewManager(db, &testutil.MockDockerClient{}, "", "")
		mgr.diskCheck = func() error { return nil }
		ctx := context.Background()
		s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
		require.NoError(t, err)
		// Expired a moment ago: the reaper may claim it, an extend may not
		// resurrect it once claimed.
		_, err = db.Exec(`UPDATE environments SET lease_expires_at = ? WHERE id = ?`, time.Now().Unix()-1, s.ID)
		require.NoError(t, err)

		var wg sync.WaitGroup
		var extendErr error
		var reaped int
		wg.Add(2)
		go func() { defer wg.Done(); _, extendErr = mgr.ExtendSandbox(ctx, testTenantID, s.ID, 600) }()
		go func() { defer wg.Done(); reaped, _ = mgr.ReapSandboxes(ctx) }()
		wg.Wait()

		_, getErr := mgr.GetSandbox(ctx, testTenantID, s.ID)
		if extendErr == nil {
			// The extend won: the row survives and was not reaped.
			assert.Zero(t, reaped)
			require.NoError(t, getErr)
		} else {
			assert.Equal(t, 1, reaped)
			require.ErrorIs(t, getErr, apierr.ErrNotFound)
		}
	}
}

func TestSandboxLogs(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	ctx := context.Background()
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	var gotTail int
	mock.LogsContainerFn = func(_ context.Context, _ string, _ bool, tail int) (io.ReadCloser, error) {
		gotTail = tail
		return io.NopCloser(strings.NewReader("log")), nil
	}
	rc, err := mgr.SandboxLogs(ctx, testTenantID, s.ID, false, 999999)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, 10000, gotTail)
	assert.Equal(t, []string{"mock-env-container-" + s.ID}, mock.LogsContainerCalls)
	assert.Equal(t, 100, ClampLogTail(0))
	assert.Equal(t, 5, ClampLogTail(5))
	_, err = mgr.SandboxLogs(ctx, otherTenantID, s.ID, false, 10)
	require.ErrorIs(t, err, apierr.ErrNotFound)
}

// Regression: environments and sandboxes share a table but not a name
// namespace. An environment named "scratch" must not block a sandbox named
// "scratch" (and the reverse), while names stay unique within each kind.
func TestSandbox_NamesAreUniquePerKind(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	ctx := context.Background()

	env, err := mgr.Create(ctx, testTenantID, CreateRequest{Name: "scratch"})
	require.NoError(t, err)
	sbx, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Name: "scratch", Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	assert.Equal(t, "scratch", sbx.Name)

	// Same kind, same name: still a conflict, and the message names the kind.
	_, err = mgr.Create(ctx, testTenantID, CreateRequest{Name: "scratch"})
	assertAPICode(t, err, 409)
	assert.Contains(t, err.Error(), "environment with name")
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Name: "scratch", Source: SandboxSource{Type: "template"}})
	assertAPICode(t, err, 409)
	assert.Contains(t, err.Error(), "sandbox with name")

	// A failed sandbox awaiting the reaper does not block an environment name.
	failed, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Name: "doomed", Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	_, err = mgr.db.Exec(`UPDATE environments SET status = 'failed' WHERE id = ?`, failed.ID)
	require.NoError(t, err)
	_, err = mgr.Create(ctx, testTenantID, CreateRequest{Name: "doomed"})
	require.NoError(t, err)

	// Kind scoping still holds for the shared names.
	_, err = mgr.Get(ctx, testTenantID, sbx.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
	_, err = mgr.GetSandbox(ctx, testTenantID, env.ID)
	require.ErrorIs(t, err, apierr.ErrNotFound)
}

func assertAPICode(t *testing.T, err error, code int) {
	t.Helper()
	require.Error(t, err)
	var apiErr *apierr.APIError
	require.True(t, errors.As(err, &apiErr), "want APIError %d, got %v", code, err)
	assert.Equal(t, code, apiErr.Code, apiErr.Message)
}
