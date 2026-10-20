// Package builder runs source-to-image builds in the ah-builder systemd unit.
package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultBuildDir = "/var/lib/ah-builds"
	BrokerSocket    = "/run/ah-build-broker/broker.sock"
)

var (
	buildIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
	refPattern     = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)
	allowedGitHost = map[string]bool{
		"github.com": true, "gitlab.com": true, "bitbucket.org": true,
		"sr.ht": true, "codeberg.org": true,
	}
)

// BuildRequest describes a build to execute.
type BuildRequest struct {
	BuildID    string `json:"build_id"`
	ServiceID  string `json:"service_id"`
	TenantID   string `json:"tenant_id"`
	SourceType string `json:"source_type"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref"`
	ImageTag   string `json:"image_tag"`
}

// Builder coordinates builds in the ah-builder systemd service.
type Builder struct {
	workDir      string
	nixpacks     string
	buildSem     chan struct{}
	tenantMu     sync.Mutex
	tenantBuilds map[string]struct{}
}

// NewBuilder creates a builder.
func NewBuilder(workDir, nixpacksPath string) (*Builder, error) {
	if err := os.MkdirAll(workDir, 0770); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	if _, err := exec.LookPath(nixpacksPath); err != nil {
		return nil, fmt.Errorf("nixpacks binary not found at %s: %w", nixpacksPath, err)
	}
	return &Builder{workDir: workDir, nixpacks: nixpacksPath, buildSem: make(chan struct{}, 3), tenantBuilds: make(map[string]struct{})}, nil
}

// Build asks the narrow systemd broker to start a fixed ah-build@ unit, then
// streams its secret-free log file until the worker exits or the request is cancelled.
func (b *Builder) Build(ctx context.Context, req BuildRequest, logCb func(string)) error {
	if err := validateBuildRequest(req); err != nil {
		return err
	}
	if !b.acquireTenant(req.TenantID) {
		return fmt.Errorf("a build is already running for this tenant")
	}
	defer b.releaseTenant(req.TenantID)
	select {
	case b.buildSem <- struct{}{}:
		defer func() { <-b.buildSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if !filepath.IsAbs(b.workDir) || filepath.Clean(b.workDir) != b.workDir {
		return fmt.Errorf("build worker root must be an absolute clean path")
	}

	buildDir := filepath.Join(b.workDir, req.BuildID)
	if err := os.Mkdir(buildDir, 0770); err != nil {
		return fmt.Errorf("create build directory: %w", err)
	}
	if err := setBuildGroup(buildDir); err != nil {
		_ = os.Remove(buildDir)
		return fmt.Errorf("set build directory group: %w", err)
	}
	if err := os.Chmod(buildDir, 0770); err != nil {
		_ = os.RemoveAll(buildDir)
		return fmt.Errorf("set build directory mode: %w", err)
	}
	cleanBuildDir := true
	defer func() {
		if cleanBuildDir {
			for _, name := range []string{"request.json", "build.log"} {
				if err := os.Remove(filepath.Join(buildDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					log.Printf("builder: failed to remove build artifact %s: %v", name, err)
					return
				}
			}
			if err := os.Remove(buildDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("builder: failed to clean build directory %s: %v", buildDir, err)
			}
		}
	}()

	manifestPath := filepath.Join(buildDir, "request.json")
	if err := writePrivateJSON(manifestPath, req, 0640); err != nil {
		return fmt.Errorf("write build request: %w", err)
	}
	if err := setBuildGroup(manifestPath); err != nil {
		return fmt.Errorf("set build request group: %w", err)
	}
	logPath := filepath.Join(buildDir, "build.log")
	if err := createBuildLog(logPath); err != nil {
		return fmt.Errorf("create build log: %w", err)
	}
	if err := setBuildGroup(logPath); err != nil {
		return fmt.Errorf("set build log group: %w", err)
	}

	if err := brokerCall(ctx, brokerRequest{Operation: "start", BuildID: req.BuildID}); err != nil {
		// A timeout may race with systemd accepting the start request. Stop the
		// fixed unit with a fresh bounded context before deciding whether cleanup is safe.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := brokerCall(stopCtx, brokerRequest{Operation: "stop", BuildID: req.BuildID})
		cancel()
		if stopErr != nil {
			cleanBuildDir = false
			return fmt.Errorf("start build worker: %w (worker stop also failed)", err)
		}
		return fmt.Errorf("start build worker: %w", err)
	}

	readLog, err := openWorkerArtifact(b.workDir, req.BuildID, "build.log", os.O_RDONLY, 0)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = brokerCall(stopCtx, brokerRequest{Operation: "stop", BuildID: req.BuildID})
		cancel()
		cleanBuildDir = false
		return fmt.Errorf("open build log: %w", err)
	}
	defer readLog.Close()
	drainer := &buildLogDrainer{reader: bufio.NewReader(readLog)}
	for {
		if _, err := drainer.drain(logCb); err != nil {
			cleanBuildDir = false
			stopBuildWorker(req.BuildID)
			return fmt.Errorf("read build log: %w", err)
		}
		if ctx.Err() != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			stopErr := brokerCall(stopCtx, brokerRequest{Operation: "stop", BuildID: req.BuildID})
			cancel()
			if stopErr != nil {
				cleanBuildDir = false
				return fmt.Errorf("build cancelled: %w (worker stop failed)", ctx.Err())
			}
			_, _ = drainer.drain(logCb)
			drainer.flush(logCb)
			return ctx.Err()
		}
		status, err := brokerStatus(ctx, req.BuildID)
		if ctx.Err() != nil {
			continue
		}
		if err != nil {
			stopErr := stopBuildWorker(req.BuildID)
			cleanBuildDir = false
			if stopErr != nil {
				return fmt.Errorf("check build worker: %w (worker stop failed)", err)
			}
			return fmt.Errorf("check build worker: %w", err)
		}
		finished := status.ActiveState == "inactive" || status.ActiveState == "failed" ||
			(status.ActiveState == "active" && status.MainPID == 0)
		if finished {
			for {
				n, drainErr := drainer.drain(logCb)
				if drainErr != nil {
					cleanBuildDir = false
					return fmt.Errorf("read completed build log: %w", drainErr)
				}
				if n == 0 {
					break
				}
			}
			drainer.flush(logCb)
			if stopErr := stopBuildWorker(req.BuildID); stopErr != nil {
				cleanBuildDir = false
				return fmt.Errorf("stop completed build worker: %w", stopErr)
			}
			if status.Result != "success" || status.ExecMainStatus != 0 {
				return fmt.Errorf("build worker failed (result=%s exit_status=%d)", status.Result, status.ExecMainStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// CancelBuild stops only the fixed unit associated with a generated build ID.
func (b *Builder) CancelBuild(buildID string) error {
	if !validBuildID(buildID) {
		return fmt.Errorf("invalid build ID")
	}
	return brokerCall(context.Background(), brokerRequest{Operation: "stop", BuildID: buildID})
}

func (b *Builder) acquireTenant(tenantID string) bool {
	b.tenantMu.Lock()
	defer b.tenantMu.Unlock()
	if _, ok := b.tenantBuilds[tenantID]; ok {
		return false
	}
	b.tenantBuilds[tenantID] = struct{}{}
	return true
}

func (b *Builder) releaseTenant(tenantID string) {
	b.tenantMu.Lock()
	defer b.tenantMu.Unlock()
	delete(b.tenantBuilds, tenantID)
}

func validateBuildRequest(req BuildRequest) error {
	if !validBuildID(req.BuildID) {
		return fmt.Errorf("invalid build ID")
	}
	if !validBuildID(req.TenantID) || !validBuildID(req.ServiceID) {
		return fmt.Errorf("invalid tenant or service ID")
	}
	if req.SourceType != "git" {
		return fmt.Errorf("unsupported source type")
	}
	if len(req.SourceURL) > 2048 {
		return fmt.Errorf("source URL too long")
	}
	u, err := url.Parse(req.SourceURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || !allowedGitHost[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("source URL must use HTTPS without credentials and a supported Git host")
	}
	if len(req.SourceRef) > 256 || !refPattern.MatchString(req.SourceRef) {
		return fmt.Errorf("invalid source ref")
	}
	wantImage := fmt.Sprintf("127.0.0.1:5000/ah/%s-%s:%s", req.TenantID, req.ServiceID, req.BuildID)
	if req.ImageTag != wantImage {
		return fmt.Errorf("invalid image tag")
	}
	return nil
}

func validBuildID(id string) bool { return buildIDPattern.MatchString(id) }

func writePrivateJSON(path string, value any, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(value)
	closeErr := f.Close()
	if encErr != nil {
		_ = os.Remove(path)
		return encErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	return os.Chmod(path, mode)
}

func createBuildLog(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0660)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0660)
}

type buildLogDrainer struct {
	reader  *bufio.Reader
	pending []byte
	discard bool
}

// drain reads at most 64 KiB per call and retains at most 16 KiB of any one
// line. This keeps log streaming from starving cancellation and unit polling.
// A line still being written by the worker (no newline yet at EOF) is held
// until a later drain completes it, or until flush once the worker is done;
// emitting it at EOF would split one log line in two.
func (d *buildLogDrainer) drain(logCb func(string)) (int, error) {
	const maxPass, maxLine = 64 * 1024, 16 * 1024
	read := 0
	for read < maxPass {
		b, err := d.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return read, nil
			}
			return read, err
		}
		read++
		if b == '\n' {
			d.emit(logCb)
			continue
		}
		if !d.discard {
			if len(d.pending) < maxLine {
				d.pending = append(d.pending, b)
			} else {
				d.discard = true
			}
		}
	}
	return read, nil
}

// flush emits a trailing unterminated line, if any. Call it only once the
// worker can no longer write.
func (d *buildLogDrainer) flush(logCb func(string)) {
	if len(d.pending) > 0 {
		d.emit(logCb)
	}
}

func (d *buildLogDrainer) emit(logCb func(string)) {
	if logCb != nil {
		logCb(sanitizeLogLine(strings.TrimSuffix(string(d.pending), "\r")))
	}
	d.pending = d.pending[:0]
	d.discard = false
}

// RunWorker executes one manifest. It is only called by ah-build@.service,
// which runs as ah-builder inside a bounded cgroup with no daemon secrets.
func RunWorker(workDir, nixpacksPath, buildID string) error {
	if !validBuildID(buildID) {
		return fmt.Errorf("invalid build ID")
	}
	if !filepath.IsAbs(workDir) || filepath.Clean(workDir) != workDir {
		return fmt.Errorf("worker build root must be an absolute clean path")
	}
	buildDir := filepath.Join(workDir, buildID)
	manifest, err := openWorkerArtifact(workDir, buildID, "request.json", os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open build request: %w", err)
	}
	var req BuildRequest
	dec := json.NewDecoder(io.LimitReader(manifest, 8*1024))
	dec.DisallowUnknownFields()
	decodeErr := dec.Decode(&req)
	var extra any
	extraErr := dec.Decode(&extra)
	closeErr := manifest.Close()
	if decodeErr != nil {
		return fmt.Errorf("invalid build request")
	}
	if !errors.Is(extraErr, io.EOF) {
		return fmt.Errorf("invalid build request")
	}
	if closeErr != nil {
		return closeErr
	}
	if err := validateBuildRequest(req); err != nil {
		return fmt.Errorf("invalid build request: %w", err)
	}
	if req.BuildID != buildID {
		return fmt.Errorf("build request ID does not match unit")
	}
	if !pathWithin(workDir, buildDir) {
		return fmt.Errorf("build directory escapes worker root")
	}

	logFile, err := openWorkerArtifact(workDir, buildID, "build.log", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open build log: %w", err)
	}
	var logMu sync.Mutex
	logCb := func(line string) {
		logMu.Lock()
		defer logMu.Unlock()
		_, _ = fmt.Fprintln(logFile, sanitizeLogLine(line))
	}
	logCb("[ah] Starting build " + req.BuildID)
	b := &Builder{workDir: workDir, nixpacks: nixpacksPath}
	err = b.executeBuild(context.Background(), req, buildDir, logCb)
	if cleanupErr := os.RemoveAll(filepath.Join(buildDir, "source")); cleanupErr != nil {
		logCb("[ah] Build source cleanup failed; operator cleanup may be required")
	}
	if err != nil {
		logCb("[ah] Build failed: " + err.Error())
	}
	if closeErr := logFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

func pathWithin(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (b *Builder) executeBuild(ctx context.Context, req BuildRequest, buildDir string, logCb func(string)) error {
	sourceDir := filepath.Join(buildDir, "source")
	if !pathWithin(b.workDir, sourceDir) {
		return fmt.Errorf("source directory escapes build root")
	}
	if err := os.Mkdir(sourceDir, 0770); err != nil {
		return fmt.Errorf("create source directory: %w", err)
	}
	if err := setBuildGroup(sourceDir); err != nil {
		_ = os.Remove(sourceDir)
		return fmt.Errorf("set source directory group: %w", err)
	}
	if err := os.Chmod(sourceDir, 0770); err != nil {
		return fmt.Errorf("set source directory mode: %w", err)
	}
	if req.SourceType != "git" {
		return fmt.Errorf("unsupported source type")
	}
	if err := b.gitClone(ctx, req, sourceDir, logCb); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if err := b.nixpacksBuild(ctx, req, sourceDir, logCb); err != nil {
		return fmt.Errorf("nixpacks build: %w", err)
	}
	if err := b.pushImage(ctx, req, logCb); err != nil {
		return fmt.Errorf("push image: %w", err)
	}
	logCb("[ah] Build succeeded: " + req.ImageTag)
	return nil
}

func stopBuildWorker(buildID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return brokerCall(ctx, brokerRequest{Operation: "stop", BuildID: buildID})
}

// sanitizeURL strips credentials from a URL for logs.
func sanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	return u.String()
}

// sanitizeLogLine strips control characters while preserving tabs.
func sanitizeLogLine(line string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
}

func isGitSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, c := range ref {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
