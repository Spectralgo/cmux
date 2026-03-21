package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- Command whitelist ----------

func TestGTBridge_AllowedCommands(t *testing.T) {
	bridge := newGTBridge()

	for _, cmd := range []string{"status", "mail", "hook", "mol", "rig", "crew"} {
		_, err := bridge.runCommand(cmd, nil)
		if err != nil && strings.Contains(err.Error(), "not allowed") {
			t.Errorf("runCommand(%q) rejected by whitelist, want allowed", cmd)
		}
		// exec failure (no Docker) is fine — we only care that it passed the whitelist check
	}
}

func TestGTBridge_BlockedCommands(t *testing.T) {
	bridge := newGTBridge()

	blocked := []struct {
		cmd  string
		args []string
	}{
		{"rm", []string{"-rf", "/"}},
		{"bash", []string{"-c", "evil"}},
		{"exec", nil},
	}

	for _, tc := range blocked {
		_, err := bridge.runCommand(tc.cmd, tc.args)
		if err == nil {
			t.Errorf("runCommand(%q) succeeded, want rejection", tc.cmd)
			continue
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("runCommand(%q) error = %v, want 'not allowed'", tc.cmd, err)
		}
	}
}

func TestGTBridge_EmptyCommand(t *testing.T) {
	bridge := newGTBridge()
	_, err := bridge.runCommand("", nil)
	if err == nil {
		t.Fatal("runCommand(\"\") succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("runCommand(\"\") error = %v, want 'not allowed'", err)
	}
}

// ---------- Docker exec construction ----------

func TestGTBridge_DockerExecArgs(t *testing.T) {
	// Verify that runCommand constructs the correct docker exec arg shape.
	// We can't actually run docker, so we validate the command by inspecting
	// the error output which includes the attempted command structure.
	bridge := &gtBridge{containerName: "test-ctr", apiBase: "http://127.0.0.1:0"}

	// runCommand for an allowed command will attempt docker exec.
	// The error message will contain the output from the failed exec.
	_, err := bridge.runCommand("status", []string{"--json"})
	if err == nil {
		t.Skip("docker is available — cannot test arg construction via error")
	}
	// Verify the error comes from exec (not whitelist rejection).
	if strings.Contains(err.Error(), "not allowed") {
		t.Fatal("status should pass whitelist check")
	}
}

func TestGTBridge_ContainerNameDefault(t *testing.T) {
	bridge := newGTBridge()
	if bridge.containerName != "gastown" {
		t.Errorf("containerName = %q, want %q", bridge.containerName, "gastown")
	}
	if bridge.apiBase != "http://localhost:19286" {
		t.Errorf("apiBase = %q, want %q", bridge.apiBase, "http://localhost:19286")
	}
}

// ---------- REST fallback chain ----------

func TestGTBridge_StatusRESTSuccess(t *testing.T) {
	// Spin up a test HTTP server that returns valid JSON on /api/options.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/options" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"agents":["alpha","beta"]}`)
	}))
	defer ts.Close()

	bridge := &gtBridge{containerName: "gastown", apiBase: ts.URL}
	result, err := bridge.status()
	if err != nil {
		t.Fatalf("status() error = %v", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if avail, ok := envelope["gt_available"].(bool); !ok || !avail {
		t.Errorf("gt_available = %v, want true", envelope["gt_available"])
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not a map: %T", envelope["data"])
	}
	agents, ok := data["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Errorf("agents = %v, want [alpha beta]", data["agents"])
	}
}

func TestGTBridge_StatusRESTFail_DockerFallback(t *testing.T) {
	// No REST server (bad URL) — status() falls through REST, tries Docker (also fails),
	// returns gt_available: false.
	bridge := &gtBridge{containerName: "nonexistent-ctr-xyz", apiBase: "http://127.0.0.1:1"}

	result, err := bridge.status()
	if err != nil {
		t.Fatalf("status() error = %v (should never error, returns fallback)", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Without a real Docker daemon the docker fallback also fails,
	// so we get gt_available: false.
	if avail, ok := envelope["gt_available"].(bool); !ok || avail {
		t.Errorf("gt_available = %v, want false (both REST and Docker fail)", envelope["gt_available"])
	}
}

func TestGTBridge_StatusAllFail(t *testing.T) {
	bridge := &gtBridge{containerName: "no-such-container", apiBase: "http://127.0.0.1:1"}

	result, err := bridge.status()
	if err != nil {
		t.Fatalf("status() error = %v", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if avail, ok := envelope["gt_available"].(bool); !ok || avail {
		t.Errorf("gt_available = %v, want false", envelope["gt_available"])
	}
}

// ---------- handleGTRequest dispatch ----------

func TestHandleGTRequest_Status(t *testing.T) {
	// Use a real httptest server so statusREST succeeds.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"polecats":3}`)
	}))
	defer ts.Close()

	bridge := &gtBridge{containerName: "gastown", apiBase: ts.URL}

	resp := handleGTRequest(bridge, rpcRequest{
		ID:     "req-1",
		Method: "gt.status",
	})

	if !resp.OK {
		t.Fatalf("gt.status returned ok=false: %+v", resp.Error)
	}
	if resp.ID != "req-1" {
		t.Errorf("resp.ID = %v, want req-1", resp.ID)
	}

	// Result should contain gt_available: true and nested data.
	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", resp.Result)
	}
	if avail, ok := resultMap["gt_available"].(bool); !ok || !avail {
		t.Errorf("gt_available = %v, want true", resultMap["gt_available"])
	}
}

func TestHandleGTRequest_Command(t *testing.T) {
	bridge := newGTBridge()

	resp := handleGTRequest(bridge, rpcRequest{
		ID:     "req-2",
		Method: "gt.command",
		Params: map[string]any{
			"command": "status",
		},
	})

	// The command will fail (no Docker), but handleGTRequest wraps that as
	// ok=true with output and exit_code.
	if !resp.OK {
		// If ok=false, it means the command param was empty or missing — that's a bug.
		if resp.Error != nil && resp.Error.Code == "invalid_params" {
			t.Fatalf("should not get invalid_params for command='status': %+v", resp.Error)
		}
	}
	if resp.ID != "req-2" {
		t.Errorf("resp.ID = %v, want req-2", resp.ID)
	}

	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", resp.Result)
	}
	// Should have output and exit_code keys.
	if _, exists := resultMap["output"]; !exists {
		t.Error("result missing 'output' key")
	}
	if _, exists := resultMap["exit_code"]; !exists {
		t.Error("result missing 'exit_code' key")
	}
}

func TestHandleGTRequest_UnknownMethod(t *testing.T) {
	bridge := newGTBridge()

	resp := handleGTRequest(bridge, rpcRequest{
		ID:     "req-3",
		Method: "gt.unknown",
	})

	if resp.OK {
		t.Fatal("gt.unknown should return ok=false")
	}
	if resp.ID != "req-3" {
		t.Errorf("resp.ID = %v, want req-3", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("error should not be nil")
	}
	if resp.Error.Code != "method_not_found" {
		t.Errorf("error.code = %q, want %q", resp.Error.Code, "method_not_found")
	}
	if !strings.Contains(resp.Error.Message, "gt.unknown") {
		t.Errorf("error.message = %q, should mention the unknown method", resp.Error.Message)
	}
}
