package services

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)

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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
	_, err := mgr.Get(context.Background(), "tenant-1", "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestListPaginated(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)

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

	mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
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

func TestIsDNSLabelSafe(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "plain lowercase name", input: "myapp", want: true},
		{name: "hyphen in middle", input: "my-app", want: true},
		{name: "single char", input: "a", want: true},
		{name: "digits only", input: "123", want: true},
		{name: "leading hyphen", input: "-myapp", want: false},
		{name: "trailing hyphen", input: "myapp-", want: false},
		{name: "uppercase letters", input: "MyApp", want: false},
		{name: "too long 64 chars", input: strings.Repeat("a", 64), want: false},
		{name: "exactly 63 chars", input: strings.Repeat("a", 63), want: true},
		{name: "empty string", input: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isDNSLabelSafe(tc.input)
			assert.Equal(t, tc.want, got, "isDNSLabelSafe(%q)", tc.input)
		})
	}
}

func TestToDNSLabel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "spaces to hyphens", input: "my app", want: "my-app"},
		{name: "mixed case and special chars", input: "My_App!", want: "my-app"},
		{name: "all hyphens", input: "---", want: ""},
		{name: "long name truncated", input: strings.Repeat("a", 70), want: strings.Repeat("a", 63)},
		{name: "trailing hyphens stripped after truncation", input: strings.Repeat("a", 62) + "!bcdef", want: strings.Repeat("a", 62)},
		{name: "already valid", input: "hello", want: "hello"},
		{name: "empty input", input: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toDNSLabel(tc.input)
			assert.Equal(t, tc.want, got, "toDNSLabel(%q)", tc.input)
		})
	}
}

func TestPublicURL(t *testing.T) {
	t.Run("with baseDomain", func(t *testing.T) {
		url := publicURL("svc-123", "my-app", "example.com")
		assert.Equal(t, "https://my-app.example.com", url)
	})

	t.Run("without baseDomain", func(t *testing.T) {
		url := publicURL("svc-123", "my-app", "")
		assert.Equal(t, "http://svc-123.localhost", url)
	})

	t.Run("baseDomain set but empty dnsLabel", func(t *testing.T) {
		url := publicURL("svc-123", "", "example.com")
		assert.Equal(t, "http://svc-123.localhost", url)
	})
}

func TestTraefikLabels_ReturnsEmpty(t *testing.T) {
	// traefikLabels now returns an empty map because routing is handled
	// by the file provider (writeTraefikRoute), not Docker labels.
	t.Run("with baseDomain returns empty", func(t *testing.T) {
		labels := traefikLabels("svc-123", "my-app", "example.com", 8080)
		assert.Empty(t, labels)
	})

	t.Run("without baseDomain returns empty", func(t *testing.T) {
		labels := traefikLabels("svc-123", "my-app", "", 8080)
		assert.Empty(t, labels)
	})
}

func TestWriteTraefikRoute(t *testing.T) {
	t.Run("writes correct config", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		dir := t.TempDir()

		mgr := NewManager(stateDB, mock, masterKey, "example.com", dir, nil)
		err := mgr.writeTraefikRoute("svc123", "tenant1", "my-app", "example.com", 8080)
		require.NoError(t, err)

		data, err := os.ReadFile(dir + "/svc123.yml")
		require.NoError(t, err)
		content := string(data)
		assert.Contains(t, content, "svc-svc123-http")
		assert.Contains(t, content, "Host(`my-app.example.com`)")
		assert.Contains(t, content, "websecure")
		assert.Contains(t, content, "redirectScheme")
		assert.Contains(t, content, "scheme: https")
		assert.Contains(t, content, "ah-tenant1-svc123")
		assert.Contains(t, content, "8080")
		assert.Contains(t, content, "letsencrypt")
	})

	t.Run("writes localhost route without baseDomain", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		dir := t.TempDir()

		mgr := NewManager(stateDB, mock, masterKey, "", dir, nil)
		err := mgr.writeTraefikRoute("svc123", "tenant1", "my-app", "", 8080)
		require.NoError(t, err)

		data, err := os.ReadFile(dir + "/svc123.yml")
		require.NoError(t, err)
		content := string(data)
		assert.Contains(t, content, "Host(`svc123.localhost`)")
		assert.Contains(t, content, "web", "localhost mode should use HTTP entrypoint")
		assert.NotContains(t, content, "websecure", "localhost mode should not use HTTPS entrypoint")
		assert.NotContains(t, content, "redirectScheme", "localhost mode should not install HTTPS redirects")
		assert.NotContains(t, content, "letsencrypt", "localhost mode should not use TLS")
		assert.Contains(t, content, "ah-tenant1-svc123")
		assert.Contains(t, content, "8080")
	})

	t.Run("no-op without traefikConfigDir", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")

		mgr := NewManager(stateDB, mock, masterKey, "example.com", "", nil)
		err := mgr.writeTraefikRoute("svc123", "tenant1", "my-app", "example.com", 8080)
		require.NoError(t, err)
	})

	t.Run("no-op without traefikConfigDir in localhost mode", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")

		mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
		err := mgr.writeTraefikRoute("svc123", "tenant1", "", "", 8080)
		require.NoError(t, err)
	})

	t.Run("localhost route uses serviceID not dnsLabel", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		dir := t.TempDir()

		mgr := NewManager(stateDB, mock, masterKey, "", dir, nil)
		// dnsLabel is empty in localhost mode (no baseDomain during Create)
		err := mgr.writeTraefikRoute("svc-abc-123", "tenant1", "", "", 3000)
		require.NoError(t, err)

		data, err := os.ReadFile(dir + "/svc-abc-123.yml")
		require.NoError(t, err)
		content := string(data)
		assert.Contains(t, content, "Host(`svc-abc-123.localhost`)")
		assert.Contains(t, content, "3000")
		assert.Contains(t, content, "ah-tenant1-svc-abc-123")
	})
}

func TestDeleteTraefikRoute(t *testing.T) {
	t.Run("deletes existing file", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		dir := t.TempDir()

		mgr := NewManager(stateDB, mock, masterKey, "example.com", dir, nil)
		// Write first
		err := mgr.writeTraefikRoute("svc123", "tenant1", "my-app", "example.com", 8080)
		require.NoError(t, err)

		// Delete
		err = mgr.deleteTraefikRoute("svc123")
		require.NoError(t, err)

		_, err = os.Stat(dir + "/svc123.yml")
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("ignores not-found", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		dir := t.TempDir()

		mgr := NewManager(stateDB, mock, masterKey, "example.com", dir, nil)
		err := mgr.deleteTraefikRoute("nonexistent")
		require.NoError(t, err)
	})
}

func TestCreateSetsDNSLabel(t *testing.T) {
	t.Run("with baseDomain", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")

		mgr := NewManager(stateDB, mock, masterKey, "example.com", "", nil)
		svc, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
			Name:  "my-service",
			Image: "nginx:latest",
			Port:  8080,
		})
		require.NoError(t, err)
		assert.Equal(t, "my-service", svc.DNSLabel, "dns_label should be derived from service name when baseDomain is set")
	})

	t.Run("without baseDomain skips dns_label", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")

		mgr := NewManager(stateDB, mock, masterKey, "", "", nil)
		svc, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{
			Name:  "my-service",
			Image: "nginx:latest",
			Port:  8080,
		})
		require.NoError(t, err)
		assert.Equal(t, "", svc.DNSLabel, "dns_label should be empty when baseDomain is not set")
	})
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
