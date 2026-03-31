package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- BT-SNAP-001: snapshot list table has correct columns ----

func TestSnapshotList_TableColumns(t *testing.T) {
	snapshots := []ahclient.Snapshot{
		{
			ID:        "aabbccdd11223344aabbccdd11223344",
			ServiceID: "dddddddd11223344dddddddd11223344",
			Name:      "pre-change-20260330",
			CreatedAt: 1743200000,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/snapshots" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(snapshots)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotList(c, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "ID", "snapshot table must have ID column")
	assert.Contains(t, output, "NAME", "snapshot table must have NAME column")
	assert.Contains(t, output, "SERVICE", "snapshot table must have SERVICE column")
	assert.Contains(t, output, "CREATED", "snapshot table must have CREATED column")
	assert.Contains(t, output, "pre-change-20260330", "snapshot table must show snapshot name")
}

// ---- BT-SNAP-002: snapshot list truncates ID ----

func TestSnapshotList_TruncatesID(t *testing.T) {
	snapshots := []ahclient.Snapshot{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "snap1", ServiceID: "dddddddd11223344dddddddd11223344", CreatedAt: 1743200000},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshots)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotList(c, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.NotContains(t, output, "aabbccdd11223344aabbccdd11223344", "full 32-char ID must not appear")
	assert.Contains(t, output, "aabbccdd", "truncated ID prefix must appear")
}

// ---- BT-SNAP-003: snapshot take creates snapshot with default name ----

func TestSnapshotTake_DefaultName(t *testing.T) {
	createCalled := false
	var capturedReq ahclient.CreateSnapshotRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/services" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
		case strings.Contains(r.URL.Path, "/snapshots") && r.Method == http.MethodPost:
			createCalled = true
			json.NewDecoder(r.Body).Decode(&capturedReq)
			json.NewEncoder(w).Encode(ahclient.Snapshot{
				ID: "snap11111111111111111111111111111", Name: capturedReq.Name, ServiceID: "aabbccdd11223344aabbccdd11223344",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotTake(c, &buf, "my-svc", "")
	require.NoError(t, err)
	assert.True(t, createCalled, "CreateSnapshot must be called")
	// Default name starts with "pre-change-"
	assert.True(t, strings.HasPrefix(capturedReq.Name, "pre-change-"),
		"default snapshot name must start with 'pre-change-', got %q", capturedReq.Name)
}

// ---- BT-SNAP-004: snapshot take uses --name when provided ----

func TestSnapshotTake_CustomName(t *testing.T) {
	var capturedReq ahclient.CreateSnapshotRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/services" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
		case strings.Contains(r.URL.Path, "/snapshots") && r.Method == http.MethodPost:
			json.NewDecoder(r.Body).Decode(&capturedReq)
			json.NewEncoder(w).Encode(ahclient.Snapshot{
				ID: "snap11111111111111111111111111111", Name: capturedReq.Name,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotTake(c, &buf, "my-svc", "my-custom-snap")
	require.NoError(t, err)
	assert.Equal(t, "my-custom-snap", capturedReq.Name, "custom --name must be used as snapshot name")
}

// ---- BT-SNAP-005: snapshot delete requires --confirm ----

func TestSnapshotDelete_RequiresConfirm(t *testing.T) {
	deleteCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		// For list call (resolveSnapshotID by name)
		json.NewEncoder(w).Encode([]ahclient.Snapshot{
			{ID: "aabbccdd11223344aabbccdd11223344", Name: "snap1"},
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runSnapshotDelete(c, "snap1", false /* confirm=false */)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm", "error must mention --confirm")
	assert.False(t, deleteCalled, "DELETE must not be called without --confirm")
}

func TestSnapshotDelete_WithConfirm(t *testing.T) {
	deleteCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/snapshots/") {
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/snapshots" {
			json.NewEncoder(w).Encode([]ahclient.Snapshot{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "snap1"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runSnapshotDelete(c, "snap1", true /* confirm=true */)
	require.NoError(t, err)
	assert.True(t, deleteCalled, "DELETE must be called when --confirm is set")
}

// ---- BT-SNAP-006: snapshot list shows "No snapshots found" for empty list ----

func TestSnapshotList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Snapshot{})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotList(c, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "No snapshots", "empty list must say 'No snapshots'")
}

// ---- Regression tests ----

// REG-SNAP-001: runSnapshotDelete with hex ID and confirm=false must error before calling DELETE
func TestSnapshotDelete_HexIDNoConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runSnapshotDelete(c, "aabbccdd11223344aabbccdd11223344", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
	assert.False(t, deleteCalled)
}

// REG-SNAP-002: snapshot take output contains snapshot name and ID
func TestSnapshotTake_OutputContainsNameAndID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/services" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
		case strings.Contains(r.URL.Path, "/snapshots") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(ahclient.Snapshot{
				ID: "snap11111111111111111111111111111", Name: "my-named-snap",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runSnapshotTake(c, &buf, "my-svc", "my-named-snap")
	require.NoError(t, err)
	output := buf.String()
	assert.Contains(t, output, "my-named-snap", "output must contain snapshot name")
}
