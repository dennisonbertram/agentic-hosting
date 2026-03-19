package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	mgr := NewManager(stateDB, mock, masterKey, "")
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

	mgr := NewManager(stateDB, mock, masterKey, "")
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

	mgr := NewManager(stateDB, mock, masterKey, "")

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

	mgr := NewManager(stateDB, mock, masterKey, "")
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

	mgr := NewManager(stateDB, mock, masterKey, "")
	_, err := mgr.Get(context.Background(), "tenant-1", "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestListPaginated(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mock := &testutil.MockDockerClient{}
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	mgr := NewManager(stateDB, mock, masterKey, "")

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

	mgr := NewManager(stateDB, mock, masterKey, "")
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
		url := publicURL("svc-123", "my-app", "tenant-1", "example.com")
		assert.Equal(t, "https://my-app.example.com", url)
	})

	t.Run("without baseDomain", func(t *testing.T) {
		url := publicURL("svc-123", "my-app", "tenant-1", "")
		assert.Equal(t, "http://svc-123.localhost", url)
	})

	t.Run("baseDomain set but empty dnsLabel", func(t *testing.T) {
		url := publicURL("svc-123", "", "tenant-1", "example.com")
		assert.Equal(t, "http://svc-123.localhost", url)
	})
}

func TestTraefikLabels(t *testing.T) {
	t.Run("with baseDomain produces TLS labels", func(t *testing.T) {
		// Use hex-format tenantID (as generated by generateID)
		labels := traefikLabels("svc-123", "my-app", "abc123def456", "example.com", 8080)

		assert.Equal(t, "true", labels["traefik.enable"])
		assert.Equal(t, "Host(`my-app.example.com`)", labels[fmt.Sprintf("traefik.http.routers.%s.rule", "svc-123")])
		assert.Equal(t, "websecure", labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", "svc-123")])
		assert.Equal(t, "true", labels[fmt.Sprintf("traefik.http.routers.%s.tls", "svc-123")])
		assert.Equal(t, "letsencrypt", labels[fmt.Sprintf("traefik.http.routers.%s.tls.certresolver", "svc-123")])
		assert.Equal(t, "8080", labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", "svc-123")])
		// traefik.docker.network should NOT be set (removed for cross-tenant isolation)
		_, hasNetwork := labels["traefik.docker.network"]
		assert.False(t, hasNetwork, "traefik.docker.network should not be present")
	})

	t.Run("without baseDomain no TLS labels", func(t *testing.T) {
		labels := traefikLabels("svc-123", "my-app", "tenant1", "", 8080)

		assert.Equal(t, "true", labels["traefik.enable"])
		assert.Equal(t, "web", labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", "svc-123")])
		// TLS labels should NOT be present
		_, hasTLS := labels[fmt.Sprintf("traefik.http.routers.%s.tls", "svc-123")]
		assert.False(t, hasTLS, "tls label should not be present without baseDomain")
		_, hasCertResolver := labels[fmt.Sprintf("traefik.http.routers.%s.tls.certresolver", "svc-123")]
		assert.False(t, hasCertResolver, "certresolver label should not be present without baseDomain")
	})

	t.Run("router key uses serviceID", func(t *testing.T) {
		labels := traefikLabels("my-svc-id", "app", "ab12cd34", "example.com", 3000)
		_, hasRouter := labels["traefik.http.routers.my-svc-id.rule"]
		assert.True(t, hasRouter, "router rule should use serviceID as key")
	})

	t.Run("tenantID not in host rule", func(t *testing.T) {
		// tenantID is no longer part of the host — only dnsLabel.baseDomain
		labels := traefikLabels("svc-1", "app", "tenant-1", "example.com", 8080)
		assert.Equal(t, "Host(`app.example.com`)", labels["traefik.http.routers.svc-1.rule"])
		assert.Equal(t, "websecure", labels["traefik.http.routers.svc-1.entrypoints"])
	})

	t.Run("invalid baseDomain falls back to localhost", func(t *testing.T) {
		labels := traefikLabels("svc-1", "app", "abc123", "example.com/evil`)", 8080)
		assert.Equal(t, "Host(`svc-1.localhost`)", labels["traefik.http.routers.svc-1.rule"])
	})

	t.Run("empty tenantID still produces domain labels", func(t *testing.T) {
		// tenantID is no longer part of the host rule
		labels := traefikLabels("svc-1", "app", "", "example.com", 8080)
		assert.Equal(t, "Host(`app.example.com`)", labels["traefik.http.routers.svc-1.rule"])
	})
}

func TestCreateSetsDNSLabel(t *testing.T) {
	t.Run("with baseDomain", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		seedTenant(t, stateDB)
		mock := &testutil.MockDockerClient{}
		masterKey := []byte("0123456789abcdef0123456789abcdef")

		mgr := NewManager(stateDB, mock, masterKey, "example.com")
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

		mgr := NewManager(stateDB, mock, masterKey, "")
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
