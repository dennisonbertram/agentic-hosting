package builder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

func (b *Builder) gitClone(ctx context.Context, req BuildRequest, sourceDir string, logCb func(string)) error {
	ref := req.SourceRef
	if ref == "" {
		ref = "main"
	}
	logCb(fmt.Sprintf("[ah] Cloning %s (ref: %s)", sanitizeURL(req.SourceURL), ref))
	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var err error
	if isGitSHA(ref) {
		logCb("[ah] Ref looks like a SHA — performing full clone then checkout")
		err = runWorkerCommand(cloneCtx, filepath.Dir(sourceDir), gitEnv(), logCb, "git", "clone", "--config", "http.followRedirects=false", req.SourceURL, sourceDir)
		if err == nil {
			err = runWorkerCommand(cloneCtx, filepath.Dir(sourceDir), gitEnv(), logCb, "git", "-C", sourceDir, "checkout", ref)
		}
	} else {
		err = runWorkerCommand(cloneCtx, filepath.Dir(sourceDir), gitEnv(), logCb, "git", "clone", "--depth=1", "--branch", ref, "--config", "http.followRedirects=false", req.SourceURL, sourceDir)
	}
	if err != nil {
		return fmt.Errorf("git clone or checkout failed")
	}
	logCb("[ah] Clone complete")
	return nil
}

func (b *Builder) nixpacksBuild(ctx context.Context, req BuildRequest, sourceDir string, logCb func(string)) error {
	logCb("[ah] Running nixpacks build...")
	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	env := []string{
		"HOME=/tmp",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"NIXPACKS_NO_CACHE=1",
		"DOCKER_CONFIG=/tmp/.docker",
	}
	if err := runWorkerCommand(buildCtx, sourceDir, env, logCb, b.nixpacks, "build", sourceDir, "--name", req.ImageTag); err != nil {
		return fmt.Errorf("nixpacks build failed")
	}
	logCb("[ah] Nixpacks build complete")
	return nil
}

func (b *Builder) pushImage(ctx context.Context, req BuildRequest, logCb func(string)) error {
	logCb("[ah] Pushing image to local registry...")
	pushCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	env := []string{"HOME=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "DOCKER_HOST=unix:///var/run/docker.sock"}
	if err := runWorkerCommand(pushCtx, b.workDir, env, logCb, "docker", "push", req.ImageTag); err != nil {
		return fmt.Errorf("docker push failed")
	}
	logCb("[ah] Push complete")
	return nil
}

func gitEnv() []string {
	return []string{"HOME=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "GIT_TERMINAL_PROMPT=0"}
}

func runWorkerCommand(ctx context.Context, dir string, env []string, logCb func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readErrs := make(chan error, 2)
	go func() { readErrs <- streamLines(stdout, logCb) }()
	go func() { readErrs <- streamLines(stderr, logCb) }()
	stdoutErr := <-readErrs
	stderrErr := <-readErrs
	waitErr := cmd.Wait()
	if stdoutErr != nil {
		return fmt.Errorf("read command output: %w", stdoutErr)
	}
	if stderrErr != nil {
		return fmt.Errorf("read command error output: %w", stderrErr)
	}
	return waitErr
}

func streamLines(r io.Reader, logCb func(string)) error {
	const maxLine = 256 * 1024
	// Read with a bufio.Reader so an overlong line is truncated instead of
	// ending the read loop (which would block the child on a full pipe).
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	truncated := false
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(buf)+len(chunk) <= maxLine {
			buf = append(buf, chunk...)
		} else {
			buf = append(buf, chunk[:maxLine-len(buf)]...)
			truncated = true
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if isPrefix {
			continue
		}
		line := sanitizeLogLine(string(buf))
		if truncated {
			line += " [line truncated]"
		}
		logCb(line)
		buf, truncated = buf[:0], false
	}
}
