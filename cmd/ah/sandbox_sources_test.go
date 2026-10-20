package main

import (
	"context"
	"errors"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/snapshots"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSnapMgr struct{ tenant string }

func (f fakeSnapMgr) Get(_ context.Context, tenantID, id string) (*snapshots.Snapshot, error) {
	if tenantID != f.tenant {
		return nil, errors.New("snapshot not found")
	}
	return &snapshots.Snapshot{ID: id, ImageRef: "img:" + id}, nil
}

func (f fakeSnapMgr) RestoreEnvVars(_ context.Context, tenantID, _ string) (map[string]string, error) {
	if tenantID != f.tenant {
		return nil, errors.New("snapshot not found")
	}
	return map[string]string{"K": "V"}, nil
}

type fakeSvcMgr struct {
	image   string
	reveals int
}

func (f *fakeSvcMgr) Get(_ context.Context, _, id string) (*services.Service, error) {
	return &services.Service{ID: id, Image: f.image}, nil
}

func (f *fakeSvcMgr) GetEnv(_ context.Context, _, _ string, reveal bool) (map[string]string, error) {
	if reveal {
		f.reveals++
	}
	return map[string]string{"DB": "x"}, nil
}

func TestSnapshotSandboxSource(t *testing.T) {
	src := snapshotSandboxSource{mgr: fakeSnapMgr{tenant: "t1"}}
	img, err := src.SnapshotImage(context.Background(), "t1", "s1")
	require.NoError(t, err)
	assert.Equal(t, "img:s1", img)
	env, err := src.SnapshotEnv(context.Background(), "t1", "s1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"K": "V"}, env)
	_, err = src.SnapshotImage(context.Background(), "t2", "s1")
	assert.Error(t, err)
}

func TestServiceSandboxSource(t *testing.T) {
	mgr := &fakeSvcMgr{image: "img"}
	src := serviceSandboxSource{mgr: mgr}
	img, env, err := src.ServiceImageAndEnv(context.Background(), "t1", "s1", false)
	require.NoError(t, err)
	assert.Equal(t, "img", img)
	assert.Nil(t, env)
	assert.Zero(t, mgr.reveals, "env is only revealed when requested")

	img, env, err = src.ServiceImageAndEnv(context.Background(), "t1", "s1", true)
	require.NoError(t, err)
	assert.Equal(t, "img", img)
	assert.Equal(t, map[string]string{"DB": "x"}, env)
	assert.Equal(t, 1, mgr.reveals)

	mgr.image = ""
	img, env, err = src.ServiceImageAndEnv(context.Background(), "t1", "s1", true)
	require.NoError(t, err)
	assert.Empty(t, img)
	assert.Nil(t, env)
}
