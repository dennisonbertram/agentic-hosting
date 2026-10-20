package main

import (
	"context"

	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/snapshots"
)

var (
	_ environments.SnapshotSource = snapshotSandboxSource{}
	_ environments.ServiceSource  = serviceSandboxSource{}
)

// snapshotSandboxSource adapts snapshots.Manager to environments.SnapshotSource.
// Lookups are tenant-scoped by the snapshot manager, so another tenant's
// snapshot is not found.
type snapshotSandboxSource struct {
	mgr interface {
		Get(ctx context.Context, tenantID, snapshotID string) (*snapshots.Snapshot, error)
		RestoreEnvVars(ctx context.Context, tenantID, snapshotID string) (map[string]string, error)
	}
}

func (s snapshotSandboxSource) SnapshotImage(ctx context.Context, tenantID, snapshotID string) (string, error) {
	snap, err := s.mgr.Get(ctx, tenantID, snapshotID)
	if err != nil {
		return "", err
	}
	return snap.ImageRef, nil
}

func (s snapshotSandboxSource) SnapshotEnv(ctx context.Context, tenantID, snapshotID string) (map[string]string, error) {
	return s.mgr.RestoreEnvVars(ctx, tenantID, snapshotID)
}

// serviceSandboxSource adapts services.Manager to environments.ServiceSource.
// It reads the service's current image directly instead of creating a
// snapshot, so snapshot retention never deletes a user's own snapshots.
type serviceSandboxSource struct {
	mgr interface {
		Get(ctx context.Context, tenantID, serviceID string) (*services.Service, error)
		GetEnv(ctx context.Context, tenantID, serviceID string, reveal bool) (map[string]string, error)
	}
}

func (s serviceSandboxSource) ServiceImageAndEnv(ctx context.Context, tenantID, serviceID string, withEnv bool) (string, map[string]string, error) {
	svc, err := s.mgr.Get(ctx, tenantID, serviceID)
	if err != nil {
		return "", nil, err
	}
	if !withEnv || svc.Image == "" {
		return svc.Image, nil, nil
	}
	env, err := s.mgr.GetEnv(ctx, tenantID, serviceID, true)
	if err != nil {
		return "", nil, err
	}
	return svc.Image, env, nil
}
