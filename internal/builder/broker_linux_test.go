//go:build linux

package builder

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBrokerUnixRoundTripUsesFixedOperations(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "broker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("AH_BUILD_BROKER_SOCKET", socketPath)
	unitTool := filepath.Join(dir, "systemctl")
	tool := "#!/bin/sh\ncase \"$1\" in\nstart|stop) exit 0 ;;\nshow) printf 'ActiveState=active\\nResult=success\\nExecMainStatus=0\\nMainPID=0\\n' ;;\n*) exit 41 ;;\nesac\n"
	if err := os.WriteFile(unitTool, []byte(tool), 0700); err != nil {
		t.Fatal(err)
	}
	server := brokerServer{systemctl: unitTool}
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()

	id := "0123456789abcdef0123456789abcdef"
	if err := brokerCall(context.Background(), brokerRequest{Operation: "start", BuildID: id}); err != nil {
		t.Fatalf("start round trip: %v", err)
	}
	status, err := brokerStatus(context.Background(), id)
	if err != nil {
		t.Fatalf("status round trip: %v", err)
	}
	if status.ActiveState != "active" || status.Result != "success" || status.ExecMainStatus != 0 || status.MainPID != 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if err := brokerCall(context.Background(), brokerRequest{Operation: "stop", BuildID: id}); err != nil {
		t.Fatalf("stop round trip: %v", err)
	}
}

func TestBrokerRequestRejectsArbitraryCommandsAndIDs(t *testing.T) {
	for _, request := range []brokerRequest{
		{Operation: "run", BuildID: "0123456789abcdef0123456789abcdef"},
		{Operation: "start", BuildID: "../../etc/passwd"},
		{Operation: "start", BuildID: "0123456789abcdef0123456789abcdeF"},
	} {
		if err := validateBrokerRequest(request); err == nil {
			t.Errorf("accepted unsafe request %+v", request)
		}
	}
}

func TestBrokerConnectionCancellationClosesRead(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "broker.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("AH_BUILD_BROKER_SOCKET", socketPath)
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		close(accepted)
		// Keep the socket open and withhold a response until the client cancels.
		<-release
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- brokerCall(ctx, brokerRequest{Operation: "status", BuildID: "0123456789abcdef0123456789abcdef"})
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("broker request was not received")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("broker read did not stop after context cancellation")
	}
	close(release)
}

func TestOpenWorkerArtifactRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkerArtifact(root, id, "request.json", os.O_RDONLY, 0); err == nil {
		t.Fatal("accepted symlink build directory")
	}
	if err := os.Remove(filepath.Join(root, id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "request.json"), filepath.Join(root, id, "request.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkerArtifact(root, id, "request.json", os.O_RDONLY, 0); err == nil {
		t.Fatal("accepted symlink build manifest")
	}
}

func TestOpenWorkerArtifactRejectsWritableManifest(t *testing.T) {
	root := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "request.json")
	if err := os.WriteFile(manifest, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifest, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkerArtifact(root, id, "request.json", os.O_RDONLY, 0); err == nil {
		t.Fatal("accepted group-writable build manifest")
	}
}

func TestOpenWorkerArtifactTraversesExecuteOnlyBuildRoot(t *testing.T) {
	root := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "request.json")
	if err := os.WriteFile(manifest, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0710); err != nil {
		t.Fatal(err)
	}
	f, err := openWorkerArtifact(root, id, "request.json", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open through execute-only root: %v", err)
	}
	_ = f.Close()
}

func TestWorkerRejectsInvalidManifestBeforeExecuting(t *testing.T) {
	root := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	request := validBuildRequest()
	request.BuildID = id
	request.ImageTag = "127.0.0.1:5000/ah/" + request.TenantID + "-" + request.ServiceID + ":" + id
	request.SourceURL = "https://attacker.invalid/repo.git"
	if err := writePrivateJSON(filepath.Join(dir, "request.json"), request, 0640); err != nil {
		t.Fatal(err)
	}
	if err := createBuildLog(filepath.Join(dir, "build.log")); err != nil {
		t.Fatal(err)
	}
	err := RunWorker(root, "/must/not/run", id)
	if err == nil {
		t.Fatal("worker accepted manifest outside source host allowlist")
	}
	if _, err := os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(err) {
		t.Fatalf("worker executed before validation, source dir stat error=%v", err)
	}
}
