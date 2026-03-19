package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// requireEnvManager is a guard that returns 503 if envManager is nil.
func (s *Server) requireEnvManager(w http.ResponseWriter) bool {
	if s.envManager == nil {
		writeError(w, http.StatusServiceUnavailable, "environment management is not available")
		return false
	}
	return true
}

func (s *Server) handleEnvironmentCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req environments.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	env, err := s.envManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) handleEnvironmentList(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	envs, err := s.envManager.ListPaginated(r.Context(), tenantID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list environments")
		return
	}
	if envs == nil {
		envs = []*environments.Environment{}
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) handleEnvironmentGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	env, err := s.envManager.Get(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleEnvironmentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Delete(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnvironmentStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Start(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleEnvironmentStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Stop(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// WebSocket upgrader for exec
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// execStartMessage is the first message sent by the client to start a command.
type execStartMessage struct {
	Type string   `json:"type"` // "start"
	Cmd  []string `json:"cmd"`
}

// execMessage represents messages exchanged during exec.
type execMessage struct {
	Type string `json:"type"`           // "stdout", "stderr", "exit", "stdin"
	Data string `json:"data,omitempty"` // base64-encoded for stdin/stdout/stderr
	Code int    `json:"code,omitempty"` // exit code
}

func (s *Server) handleEnvironmentExec(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	containerID, err := s.envManager.GetContainerID(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("environments: websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Read first message to get command
	var startMsg execStartMessage
	if err := conn.ReadJSON(&startMsg); err != nil {
		conn.WriteJSON(execMessage{Type: "error", Data: "failed to read start message"})
		return
	}
	if startMsg.Type != "start" || len(startMsg.Cmd) == 0 {
		conn.WriteJSON(execMessage{Type: "error", Data: "first message must be type=start with cmd"})
		return
	}

	// Touch activity
	s.envManager.TouchActivity(r.Context(), envID)

	// Create exec
	if s.docker == nil {
		conn.WriteJSON(execMessage{Type: "error", Data: "docker not available"})
		return
	}

	execID, err := s.docker.ExecCreate(r.Context(), containerID, startMsg.Cmd, true)
	if err != nil {
		conn.WriteJSON(execMessage{Type: "error", Data: fmt.Sprintf("exec create failed: %v", err)})
		return
	}

	reader, writer, err := s.docker.ExecAttach(r.Context(), execID)
	if err != nil {
		conn.WriteJSON(execMessage{Type: "error", Data: fmt.Sprintf("exec attach failed: %v", err)})
		return
	}
	defer reader.Close()

	// Copy Docker stdout -> WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				s.envManager.TouchActivity(r.Context(), envID)
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Copy WebSocket stdin -> Docker
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if _, err := writer.Write(msg); err != nil {
				return
			}
		}
	}()

	<-done

	// Get exit code
	exitCode, _, _ := s.docker.ExecInspect(r.Context(), execID)
	conn.WriteJSON(execMessage{Type: "exit", Code: exitCode})
}

func (s *Server) handleEnvironmentFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	containerID, err := s.envManager.GetContainerID(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Limit upload to 50MB
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)

	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	// Get destination path
	dstPath := r.FormValue("path")
	if dstPath == "" {
		dstPath = "/workspace"
	}

	// Validate path is under /workspace
	clean := filepath.Clean(dstPath)
	if !strings.HasPrefix(clean, "/workspace") {
		writeError(w, http.StatusBadRequest, "path must be under /workspace")
		return
	}

	// Create tar archive with the file
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: header.Filename,
		Size: int64(len(data)),
		Mode: 0644,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create tar header")
		return
	}
	if _, err := tw.Write(data); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write tar data")
		return
	}
	tw.Close()

	if err := s.docker.CopyToContainer(r.Context(), containerID, clean, &buf); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to copy file: %v", err))
		return
	}

	s.envManager.TouchActivity(r.Context(), envID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "uploaded", "path": clean + "/" + header.Filename})
}

func (s *Server) handleEnvironmentFileDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	containerID, err := s.envManager.GetContainerID(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Extract path from wildcard
	filePath := chi.URLParam(r, "*")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "file path required")
		return
	}

	// Validate path
	clean := filepath.Clean("/workspace/" + filePath)
	if !strings.HasPrefix(clean, "/workspace") {
		writeError(w, http.StatusBadRequest, "path must be under /workspace")
		return
	}

	rc, err := s.docker.CopyFromContainer(r.Context(), containerID, clean)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	defer rc.Close()

	// Extract file from tar archive
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err != nil {
			writeError(w, http.StatusNotFound, "file not found in archive")
			return
		}
		if hdr.Typeflag == tar.TypeReg {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(hdr.Name)))
			w.Header().Set("Content-Type", "application/octet-stream")
			io.Copy(w, tr)
			s.envManager.TouchActivity(r.Context(), envID)
			return
		}
	}
}
