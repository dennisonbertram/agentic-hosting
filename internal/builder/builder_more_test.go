package builder

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkReader returns its chunks one Read at a time with an io.EOF between
// them, like a log file the worker is still appending to.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	c := r.chunks[0]
	if c == "" { // explicit EOF marker
		r.chunks = r.chunks[1:]
		return 0, io.EOF
	}
	n := copy(p, c)
	if n == len(c) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = c[n:]
	}
	return n, nil
}

func TestBuildLogDrainer_HoldsPartialLineUntilComplete(t *testing.T) {
	r := &chunkReader{chunks: []string{"first\nsec", "", "ond\r\nthi", "", "rd"}}
	d := &buildLogDrainer{reader: bufio.NewReader(r)}
	var got []string
	cb := func(l string) { got = append(got, l) }

	_, err := d.drain(cb)
	require.NoError(t, err)
	assert.Equal(t, []string{"first"}, got, "a line without its newline must not be emitted at EOF")

	_, err = d.drain(cb)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, got, "CR is trimmed and the split line is joined")

	_, err = d.drain(cb)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, got)

	d.flush(cb)
	assert.Equal(t, []string{"first", "second", "third"}, got, "flush emits the trailing line once the worker is done")
	d.flush(cb)
	assert.Len(t, got, 3, "flush with nothing pending is a no-op")
}

func TestBuildLogDrainer_TruncatesOverlongLineAndStripsControls(t *testing.T) {
	long := strings.Repeat("y", 20*1024)
	d := &buildLogDrainer{reader: bufio.NewReader(strings.NewReader(long + "\nok\x1b[31m\tx\n"))}
	var got []string
	for {
		n, err := d.drain(func(l string) { got = append(got, l) })
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	require.Len(t, got, 2)
	assert.Len(t, got[0], 16*1024, "line is capped at 16 KiB")
	assert.Equal(t, "ok[31m\tx", got[1], "control characters except tab are removed")
}

func TestBuildLogDrainer_BareCRStreamsProgressAndCRLFCountsOnce(t *testing.T) {
	// Progress output that only redraws with '\r' must stream per update, and
	// a CRLF split across reads must not produce an extra empty line.
	r := &chunkReader{chunks: []string{"10%\r20%\r30%", "", "\r", "", "\ndone\n\nlast\r"}}
	d := &buildLogDrainer{reader: bufio.NewReader(r)}
	var got []string
	cb := func(l string) { got = append(got, l) }
	for i := 0; i < 3; i++ {
		_, err := d.drain(cb)
		require.NoError(t, err)
	}
	d.flush(cb)
	assert.Equal(t, []string{"10%", "20%", "30%", "done", "", "last"}, got)
}

// errAfterReader returns data, then a non-EOF error.
type errAfterReader struct {
	data string
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestBuildLogDrainer_FlushAfterReadErrorKeepsPartialLine(t *testing.T) {
	d := &buildLogDrainer{reader: bufio.NewReader(&errAfterReader{data: "ok\npartial"})}
	var got []string
	cb := func(l string) { got = append(got, l) }
	_, err := d.drain(cb)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	d.flush(cb)
	assert.Equal(t, []string{"ok", "partial"}, got)
}

func TestBuildLogDrainer_NilCallback(t *testing.T) {
	d := &buildLogDrainer{reader: bufio.NewReader(strings.NewReader("a\nb"))}
	n, err := d.drain(nil)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	d.flush(nil)
	assert.Empty(t, d.pending)
}

func TestSanitizeURL(t *testing.T) {
	assert.Equal(t, "https://github.com/a/b", sanitizeURL("https://user:secret@github.com/a/b"))
	assert.Equal(t, "<invalid-url>", sanitizeURL("https://[::1"))
}

func TestPathWithin(t *testing.T) {
	assert.True(t, pathWithin("/var/lib/ah-builds", "/var/lib/ah-builds/abc/source"))
	assert.True(t, pathWithin("/var/lib/ah-builds", "/var/lib/ah-builds"))
	assert.False(t, pathWithin("/var/lib/ah-builds", "/var/lib/ah-builds/../etc"))
	assert.False(t, pathWithin("/var/lib/ah-builds", "/var/lib/ah-builds-evil/x"))
}

func TestTenantBuildSlot(t *testing.T) {
	b := &Builder{tenantBuilds: map[string]struct{}{}}
	assert.True(t, b.acquireTenant("t1"))
	assert.False(t, b.acquireTenant("t1"), "only one build per tenant at a time")
	assert.True(t, b.acquireTenant("t2"))
	b.releaseTenant("t1")
	assert.True(t, b.acquireTenant("t1"))
}

func TestValidateBuildRequest_MoreRejections(t *testing.T) {
	tests := map[string]func(*BuildRequest){
		"invalid tenant":  func(r *BuildRequest) { r.TenantID = "x" },
		"invalid service": func(r *BuildRequest) { r.ServiceID = "x" },
		"url too long":    func(r *BuildRequest) { r.SourceURL = "https://github.com/" + strings.Repeat("a", 2048) },
		"ref too long":    func(r *BuildRequest) { r.SourceRef = strings.Repeat("a", 257) },
		"empty ref":       func(r *BuildRequest) { r.SourceRef = "" },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			req := validBuildRequest()
			edit(&req)
			assert.Error(t, validateBuildRequest(req))
		})
	}
}

func TestRunWorkerCommand(t *testing.T) {
	// stdout and stderr are streamed concurrently, so the callback must be safe
	// for concurrent use (RunWorker's is mutex-guarded too).
	var mu sync.Mutex
	var lines []string
	err := runWorkerCommand(context.Background(), t.TempDir(), []string{"PATH=/usr/bin:/bin"}, func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() },
		"sh", "-c", "echo out; echo err >&2")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"out", "err"}, lines)

	err = runWorkerCommand(context.Background(), t.TempDir(), []string{"PATH=/usr/bin:/bin"}, func(string) {}, "sh", "-c", "exit 3")
	assert.Error(t, err)

	err = runWorkerCommand(context.Background(), t.TempDir(), nil, func(string) {}, "/nonexistent/binary")
	assert.Error(t, err)
}

func TestGitEnvDisablesPrompts(t *testing.T) {
	assert.Contains(t, gitEnv(), "GIT_TERMINAL_PROMPT=0")
}

func TestNewBuilder(t *testing.T) {
	_, err := NewBuilder(filepath.Join(t.TempDir(), "work"), "/nonexistent/nixpacks")
	assert.Error(t, err)

	b, err := NewBuilder(filepath.Join(t.TempDir(), "work"), "sh")
	require.NoError(t, err)
	assert.Equal(t, 3, cap(b.buildSem))
}

func TestCancelBuild_RejectsInvalidID(t *testing.T) {
	b := &Builder{}
	assert.EqualError(t, b.CancelBuild("../x"), "invalid build ID")
}

func TestBuild_RejectsInvalidRequestAndBusyTenant(t *testing.T) {
	b := &Builder{workDir: t.TempDir(), buildSem: make(chan struct{}, 1), tenantBuilds: map[string]struct{}{}}
	bad := validBuildRequest()
	bad.SourceType = "zip"
	assert.Error(t, b.Build(context.Background(), bad, nil))

	req := validBuildRequest()
	require.True(t, b.acquireTenant(req.TenantID))
	assert.EqualError(t, b.Build(context.Background(), req, nil), "a build is already running for this tenant")
	b.releaseTenant(req.TenantID)
}

func TestBuild_RejectsUncleanWorkDir(t *testing.T) {
	b := &Builder{workDir: "relative/dir", buildSem: make(chan struct{}, 1), tenantBuilds: map[string]struct{}{}}
	assert.Error(t, b.Build(context.Background(), validBuildRequest(), nil))
	assert.Empty(t, b.tenantBuilds, "tenant slot is released on every exit")
}

func TestBuild_CleansUpWhenBuildGroupUnavailable(t *testing.T) {
	if _, err := user.LookupGroup("ah-build"); err == nil {
		t.Skip("ah-build group exists on this host; this test covers hosts without it")
	}
	work := t.TempDir()
	b := &Builder{workDir: work, buildSem: make(chan struct{}, 1), tenantBuilds: map[string]struct{}{}}
	req := validBuildRequest()
	err := b.Build(context.Background(), req, nil)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(work, req.BuildID))
	assert.True(t, os.IsNotExist(statErr), "build directory is removed after setup failure")
}

func TestRunWorker_RejectsBadInputs(t *testing.T) {
	id := strings.Repeat("c", 32)
	assert.EqualError(t, RunWorker("/tmp", "nixpacks", "bad"), "invalid build ID")
	assert.Error(t, RunWorker("relative", "nixpacks", id))
	assert.Error(t, RunWorker("/tmp/../tmp", "nixpacks", id))
	assert.Error(t, RunWorker(t.TempDir(), "nixpacks", id), "missing manifest")
}

func TestExecuteBuild_RejectsEscapingSourceDir(t *testing.T) {
	b := &Builder{workDir: t.TempDir()}
	err := b.executeBuild(context.Background(), validBuildRequest(), "/elsewhere", func(string) {})
	assert.EqualError(t, err, "source directory escapes build root")
}
