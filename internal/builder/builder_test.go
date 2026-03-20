package builder

import (
	"strings"
	"testing"
)

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
		{"abc123g", false},   // 'g' is not hex
		{"abc123G", false},   // uppercase G, but also: uppercase letters are not valid hex lower
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
