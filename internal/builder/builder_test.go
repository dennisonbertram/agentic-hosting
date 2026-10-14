package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func validBuildRequest() BuildRequest {
	tenantID := strings.Repeat("a", 32)
	serviceID := strings.Repeat("b", 32)
	buildID := strings.Repeat("c", 32)
	return BuildRequest{
		BuildID: buildID, TenantID: tenantID, ServiceID: serviceID,
		SourceType: "git", SourceURL: "https://github.com/example/repo.git",
		SourceRef: "main", ImageTag: fmt.Sprintf("127.0.0.1:5000/ah/%s-%s:%s", tenantID, serviceID, buildID),
	}
}

func TestValidateBuildRequest(t *testing.T) {
	valid := validBuildRequest()
	if err := validateBuildRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*BuildRequest)
	}{
		{"invalid build ID", func(r *BuildRequest) { r.BuildID = "../../etc" }},
		{"unsupported source", func(r *BuildRequest) { r.SourceType = "archive" }},
		{"http source", func(r *BuildRequest) { r.SourceURL = "http://github.com/example/repo" }},
		{"userinfo source", func(r *BuildRequest) { r.SourceURL = "https://token@github.com/example/repo" }},
		{"untrusted source host", func(r *BuildRequest) { r.SourceURL = "https://github.evil.test/example/repo" }},
		{"unsafe ref", func(r *BuildRequest) { r.SourceRef = "main;id" }},
		{"unrelated image target", func(r *BuildRequest) { r.ImageTag = "attacker.invalid/example:latest" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.edit(&request)
			if err := validateBuildRequest(request); err == nil {
				t.Fatal("expected request to be rejected")
			}
		})
	}
}

func TestBuildIDValidation(t *testing.T) {
	for _, id := range []string{strings.Repeat("0", 32), strings.Repeat("f", 32)} {
		if !validBuildID(id) {
			t.Errorf("valid generated build ID rejected: %q", id)
		}
	}
	for _, id := range []string{"", "../" + strings.Repeat("a", 32), strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if validBuildID(id) {
			t.Errorf("invalid build ID accepted: %q", id)
		}
	}
}

func TestBuildArtifactsRemainGroupReadableUnderServiceUmask(t *testing.T) {
	dir := t.TempDir()
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	manifest := filepath.Join(dir, "request.json")
	if err := writePrivateJSON(manifest, validBuildRequest(), 0640); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "build.log")
	if err := createBuildLog(logPath); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{manifest: 0640, logPath: 0660} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
		}
	}
}

func TestIsGitSHA(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		// Clearly branch names — must be false
		{"main", false},
		{"master", false},
		{"HEAD", false},
		{"feature/foo", false},
		{"v1.0.0", false},
		{"release-2024", false},

		// Too short (< 7 chars) — false even if all hex
		{"abc123", false},
		{"", false},
		{"a", false},

		// Too long (> 40 chars) — false
		{strings.Repeat("a", 41), false},

		// Contains non-hex chars — false
		{"abc123g", false}, // 'g' is not hex
		{"abc123G", false}, // uppercase G, but also: uppercase letters are not valid hex lower
		{"abcdefg1234567", false},
		{"ABCDEF1234567", false}, // uppercase hex — our helper requires lowercase

		// Valid abbreviated SHA (7 chars)
		{"abc1234", true},
		{"0000000", true},
		{"deadbee", true},
		{"1234567", true},

		// Valid SHA between 7 and 40 chars
		{"abc1234def567", true},
		{"abcdef1234567890abcd", true},

		// Valid full 40-char SHA
		{strings.Repeat("a", 40), true},
		{"a" + strings.Repeat("b", 38) + "c", true},
		{"0000000000000000000000000000000000000000", true},
		{"abcdef0123456789abcdef0123456789abcdef01", true},

		// Exactly 40 chars but contains non-hex
		{"abcdef0123456789abcdef0123456789abcdefgg", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.ref, func(t *testing.T) {
			got := isGitSHA(tc.ref)
			if got != tc.want {
				t.Errorf("isGitSHA(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestGitCloneArgs_BranchVsSHA verifies that the builder selects the correct
// git strategy based on whether the ref looks like a SHA or a branch name.
// We do this by inspecting isGitSHA directly — the integration (systemd-run)
// is exercised in the real clone path, which requires a live server.
func TestGitCloneArgs_BranchVsSHA(t *testing.T) {
	// Branch refs must NOT be treated as SHAs (uses --depth=1 --branch path)
	branchRefs := []string{"main", "master", "develop", "feature/login", "v1.2.3"}
	for _, ref := range branchRefs {
		if isGitSHA(ref) {
			t.Errorf("ref %q was incorrectly detected as a SHA; should use --depth=1 --branch path", ref)
		}
	}

	// SHA refs must be detected as SHAs (uses full clone + checkout path)
	shaRefs := []string{
		"abc1234",
		"deadbeef",
		"abcdef0123456789abcdef0123456789abcdef01",
		"0000000000000000000000000000000000000000",
	}
	for _, ref := range shaRefs {
		if !isGitSHA(ref) {
			t.Errorf("ref %q was not detected as a SHA; should use full-clone+checkout path", ref)
		}
	}
}
