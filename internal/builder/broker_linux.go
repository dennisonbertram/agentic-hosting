//go:build linux

package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const brokerTimeout = 12 * time.Second

func brokerSocketPath() string {
	if path := os.Getenv("AH_BUILD_BROKER_SOCKET"); path != "" {
		return path
	}
	return BrokerSocket
}

func brokerCall(ctx context.Context, request brokerRequest) error {
	_, err := brokerDo(ctx, request)
	return err
}

func brokerStatus(ctx context.Context, buildID string) (brokerResponse, error) {
	return brokerDo(ctx, brokerRequest{Operation: "status", BuildID: buildID})
}

// brokerDo performs one broker exchange and turns a refused request into an error.
func brokerDo(ctx context.Context, request brokerRequest) (brokerResponse, error) {
	response, err := brokerExchange(ctx, request)
	if err != nil {
		return brokerResponse{}, err
	}
	if !response.OK {
		return brokerResponse{}, errors.New(response.Error)
	}
	return response, nil
}

func brokerExchange(ctx context.Context, request brokerRequest) (brokerResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateBrokerRequest(request); err != nil {
		return brokerResponse{}, err
	}
	boundedCtx, cancel := context.WithTimeout(ctx, brokerTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(boundedCtx, "unix", brokerSocketPath())
	if err != nil {
		return brokerResponse{}, fmt.Errorf("connect to build broker: %w", err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(boundedCtx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := boundedCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return brokerResponse{}, fmt.Errorf("send build broker request: %w", err)
	}
	if unixConn, ok := conn.(*net.UnixConn); ok {
		if err := unixConn.CloseWrite(); err != nil {
			return brokerResponse{}, fmt.Errorf("finish build broker request: %w", err)
		}
	}
	var response brokerResponse
	dec := json.NewDecoder(io.LimitReader(conn, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&response); err != nil {
		return brokerResponse{}, fmt.Errorf("read build broker response: %w", err)
	}
	return response, nil
}

func validateBrokerRequest(request brokerRequest) error {
	if !validBuildID(request.BuildID) {
		return fmt.Errorf("invalid build ID")
	}
	switch request.Operation {
	case "start", "stop", "status":
		return nil
	default:
		return fmt.Errorf("unsupported build broker operation")
	}
}

// openWorkerArtifact resolves only a regular file beneath the configured build
// root. O_NOFOLLOW on each open prevents symlink redirection from a source tree.
func openWorkerArtifact(root, buildID, name string, flags int, mode uint32) (*os.File, error) {
	if !validBuildID(buildID) || (name != "request.json" && name != "build.log") {
		return nil, fmt.Errorf("invalid build artifact")
	}
	// O_PATH permits traversal through an execute-only 0710 build root without
	// granting ah-builder directory listing access.
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	dirFD, err := unix.Openat(rootFD, buildID, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("build artifact is not a regular file")
	}
	if name == "request.json" && info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, fmt.Errorf("build request is writable by its reader")
	}
	return f, nil
}

func setBuildGroup(path string) error {
	group, err := user.LookupGroup("ah-build")
	if err != nil {
		return fmt.Errorf("lookup ah-build group: %w", err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("parse ah-build GID: %w", err)
	}
	return os.Chown(path, -1, gid)
}

// RunBroker serves fixed start/stop/status operations for ah-build@ units.
func RunBroker(ctx context.Context, socketPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ahGroup, err := user.LookupGroup("ah")
	if err != nil {
		return fmt.Errorf("lookup ah group: %w", err)
	}
	gid, err := strconv.Atoi(ahGroup.Gid)
	if err != nil {
		return fmt.Errorf("parse ah GID: %w", err)
	}
	if !filepath.IsAbs(socketPath) {
		return fmt.Errorf("broker socket path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return fmt.Errorf("create broker socket directory: %w", err)
	}
	dirInfo, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil {
		return fmt.Errorf("verify broker socket directory: %w", err)
	}
	dirStat, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirStat.Uid != 0 || dirStat.Gid != uint32(gid) || dirInfo.Mode().Perm() != 0750 {
		return fmt.Errorf("broker socket directory ownership or permissions are invalid")
	}
	if info, err := os.Lstat(socketPath); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 {
			return fmt.Errorf("refusing to replace unexpected broker socket path")
		}
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("remove stale broker socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	addr := &net.UnixAddr{Name: socketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("listen for build broker: %w", err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(socketPath) }()
	go func() { <-ctx.Done(); _ = listener.Close() }()
	if err := os.Chmod(socketPath, 0660); err != nil {
		return fmt.Errorf("set broker socket mode: %w", err)
	}
	if err := os.Chown(socketPath, 0, gid); err != nil {
		return fmt.Errorf("set broker socket group: %w", err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		return fmt.Errorf("verify broker socket: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || stat.Uid != 0 || stat.Gid != uint32(gid) || info.Mode().Perm() != 0660 {
		return fmt.Errorf("broker socket ownership or permissions are invalid")
	}
	broker := brokerServer{systemctl: "/usr/bin/systemctl"}
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return fmt.Errorf("accept build broker connection: %w", err)
		}
		go broker.serve(conn)
	}
}

type brokerServer struct {
	systemctl string
}

func (b brokerServer) serve(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(brokerTimeout))
	dec := json.NewDecoder(io.LimitReader(conn, 4096))
	dec.DisallowUnknownFields()
	var request brokerRequest
	if err := dec.Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(brokerResponse{Error: "invalid build broker request"})
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		_ = json.NewEncoder(conn).Encode(brokerResponse{Error: "invalid build broker request"})
		return
	}
	if err := validateBrokerRequest(request); err != nil {
		_ = json.NewEncoder(conn).Encode(brokerResponse{Error: "invalid build broker request"})
		return
	}
	response, err := b.perform(request)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(brokerResponse{Error: "build worker operation failed"})
		return
	}
	response.OK = true
	_ = json.NewEncoder(conn).Encode(response)
}

func (b brokerServer) perform(request brokerRequest) (brokerResponse, error) {
	unit := "ah-build@" + request.BuildID + ".service"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch request.Operation {
	case "start":
		if _, err := b.execute(ctx, "start", unit); err != nil {
			return brokerResponse{}, err
		}
		return brokerResponse{ActiveState: "active"}, nil
	case "stop":
		if _, err := b.execute(ctx, "stop", unit); err != nil {
			return brokerResponse{}, err
		}
		return brokerResponse{ActiveState: "inactive", Result: "success"}, nil
	case "status":
		output, err := b.execute(ctx, "show", "--no-pager", "--property=ActiveState,Result,ExecMainStatus,MainPID", unit)
		if err != nil {
			return brokerResponse{}, err
		}
		values := map[string]string{}
		for _, line := range strings.Split(output, "\n") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				values[parts[0]] = parts[1]
			}
		}
		status, err := strconv.Atoi(values["ExecMainStatus"])
		if err != nil {
			return brokerResponse{}, fmt.Errorf("invalid unit status")
		}
		if values["ActiveState"] == "" || values["Result"] == "" {
			return brokerResponse{}, fmt.Errorf("unit status unavailable")
		}
		pid, err := strconv.Atoi(values["MainPID"])
		if err != nil || pid < 0 {
			return brokerResponse{}, fmt.Errorf("invalid unit main PID")
		}
		return brokerResponse{ActiveState: values["ActiveState"], Result: values["Result"], ExecMainStatus: status, MainPID: pid}, nil
	default:
		return brokerResponse{}, fmt.Errorf("unsupported operation")
	}
}

func (b brokerServer) execute(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.systemctl, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	stdout, stderr := &boundedBuffer{}, &boundedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("systemctl operation failed")
	}
	return stdout.String(), nil
}

type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const limit = 8192
	if b.buffer.Len() < limit {
		n := limit - b.buffer.Len()
		if n > len(p) {
			n = len(p)
		}
		_, _ = b.buffer.Write(p[:n])
	}
	return len(p), nil
}
func (b *boundedBuffer) String() string { return b.buffer.String() }
