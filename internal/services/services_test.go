package services

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateImage_AllowsPlatformLocalRegistry(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "docker hub image", image: "nginx:latest"},
		{name: "docker hub namespace image", image: "library/nginx:latest"},
		{name: "loopback registry by ip", image: "127.0.0.1:5000/tenant/my-app:latest"},
		{name: "loopback registry by localhost", image: "localhost:5000/tenant/my-app:latest"},
		{name: "build output image tag", image: "127.0.0.1:5000/ah/tenant-service:build123"},
		{name: "external registry remains blocked", image: "evil.example.com/team/app:latest", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImage(tc.image)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateImage(%q) unexpectedly succeeded", tc.image)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateImage(%q) returned error: %v", tc.image, err)
			}
		})
	}
}

func TestCreate_Success(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)
	svc, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "my-service",
		Image: "nginx:latest",
		Port:  8080,
	})
	require.NoError(t, err)
	assert.Equal(t, "my-service", svc.Name)
	assert.Equal(t, "created", svc.Status)
	assert.Equal(t, 8080, svc.Port)
	assert.NotEmpty(t, svc.ID)
}

func TestCreate_DefaultPort(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)
	svc, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "my-service",
		Image: "nginx:latest",
	})
	require.NoError(t, err)
	assert.Equal(t, 8000, svc.Port, "should default to 8000")
}

func TestCreate_QuotaExceeded(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenantWithQuota(t, stateDB, 1)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)

	// Create first service
	_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "svc-1",
		Image: "nginx:latest",
	})
	require.NoError(t, err)

	// Second should hit quota
	_, err = mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "svc-2",
		Image: "nginx:latest",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrQuotaExceeded))
}

func TestCreate_InvalidImage(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)
	_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "my-service",
		Image: "evil.example.com/malware:latest",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrValidation))
}

func TestGet_NotFound(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)
	_, err := mgr.Get(context.Background(), "tenant-1", "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestListPaginated(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)

	// Create 3 services
	for i := 0; i < 3; i++ {
		_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
			Name:  "svc-" + string(rune('a'+i)),
			Image: "nginx:latest",
		})
		require.NoError(t, err)
	}

	// List with limit 2
	svcs, err := mgr.ListPaginated(context.Background(), "tenant-1", 2, 0)
	require.NoError(t, err)
	assert.Len(t, svcs, 2)

	// List with offset 2
	svcs, err = mgr.ListPaginated(context.Background(), "tenant-1", 10, 2)
	require.NoError(t, err)
	assert.Len(t, svcs, 1)
}

func TestDelete_Success(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey)
	svc, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
		Name:  "my-service",
		Image: "nginx:latest",
	})
	require.NoError(t, err)

	err = mgr.Delete(context.Background(), "tenant-1", svc.ID)
	require.NoError(t, err)

	// Should be gone
	_, err = mgr.Get(context.Background(), "tenant-1", svc.ID)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestValidateEnvVars(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantErr bool
	}{
		{name: "valid", vars: map[string]string{"FOO": "bar"}, wantErr: false},
		{name: "invalid key", vars: map[string]string{"123BAD": "bar"}, wantErr: true},
		{name: "empty key", vars: map[string]string{"": "bar"}, wantErr: true},
		{name: "null in value", vars: map[string]string{"FOO": "bar\x00baz"}, wantErr: true},
		{name: "denied key", vars: map[string]string{"PATH": "/bin"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEnvVars(tc.vars)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func seedTenant(t *testing.T, db *sql.DB) {
	t.Helper()
	seedTenantWithQuota(t, db, 10)
}

func seedTenantWithQuota(t *testing.T, db *sql.DB, maxServices int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, max_services) VALUES (?, ?)`, "tenant-1", maxServices)
	if err != nil {
		t.Fatalf("seed quota: %v", err)
	}
}
