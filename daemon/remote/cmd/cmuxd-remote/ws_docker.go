package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// gtBridge provides Gas Town dashboard data and command execution.
type gtBridge struct {
	containerName string // Docker container running Gas Town (default: "gastown")
	apiBase       string // REST API base (default: "http://localhost:19286")
}

// allowedGTCommands is the whitelist of commands permitted via gt.command.
// Prevents arbitrary exec inside the Gas Town container.
var allowedGTCommands = map[string]bool{
	"status": true,
	"mail":   true,
	"hook":   true,
	"mol":    true,
	"rig":    true,
	"crew":   true,
}

// newGTBridge creates a bridge with default settings.
func newGTBridge() *gtBridge {
	return &gtBridge{
		containerName: "gastown",
		apiBase:       "http://localhost:19286",
	}
}

// status returns Gas Town dashboard data. It tries the REST API first,
// then falls back to docker exec, and returns {gt_available: false} if both fail.
func (b *gtBridge) status() (json.RawMessage, error) {
	// Try REST first.
	result, err := b.statusREST()
	if err == nil {
		return result, nil
	}

	// Fallback: docker exec gt status --json.
	result, err = b.statusDocker()
	if err == nil {
		return result, nil
	}

	// Both failed.
	msg, _ := json.Marshal(map[string]any{"gt_available": false})
	return msg, nil
}

// statusREST fetches GT status via the REST API.
func (b *gtBridge) statusREST() (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.apiBase+"/api/options", nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gt REST returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, err
	}

	// Wrap in gt_available envelope.
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	envelope := map[string]any{
		"gt_available": true,
		"data":         parsed,
	}
	return json.Marshal(envelope)
}

// statusDocker fetches GT status via docker exec.
func (b *gtBridge) statusDocker() (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "exec", b.containerName, "gt", "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker exec gt status: %w", err)
	}

	// Validate JSON.
	var parsed any
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("docker exec gt status: invalid JSON: %w", err)
	}

	envelope := map[string]any{
		"gt_available": true,
		"data":         parsed,
	}
	return json.Marshal(envelope)
}

// available returns true if Gas Town is reachable via REST.
func (b *gtBridge) available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.apiBase+"/api/options", nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// runCommand executes a whitelisted gt command via docker exec.
func (b *gtBridge) runCommand(cmd string, args []string) (string, error) {
	if !allowedGTCommands[cmd] {
		return "", fmt.Errorf("command %q is not allowed", cmd)
	}

	execArgs := []string{"exec", "-i", b.containerName, "gt", cmd}
	execArgs = append(execArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, "docker", execArgs...)
	out, err := command.CombinedOutput()
	if err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		return string(out), fmt.Errorf("exit %d: %s", exitCode, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// handleGTRequest routes gt.status and gt.command RPC requests.
func handleGTRequest(bridge *gtBridge, req rpcRequest) rpcResponse {
	switch req.Method {
	case "gt.status":
		result, err := bridge.status()
		if err != nil {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "gt_error",
					Message: err.Error(),
				},
			}
		}
		// result is already json.RawMessage; unmarshal to any for the response.
		var parsed any
		_ = json.Unmarshal(result, &parsed)
		return rpcResponse{
			ID:     req.ID,
			OK:     true,
			Result: parsed,
		}

	case "gt.command":
		cmd, ok := getStringParam(req.Params, "command")
		if !ok || cmd == "" {
			return rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "invalid_params",
					Message: "command parameter is required",
				},
			}
		}

		var args []string
		if rawArgs, exists := req.Params["args"]; exists {
			if argSlice, ok := rawArgs.([]any); ok {
				for _, a := range argSlice {
					if s, ok := a.(string); ok {
						args = append(args, s)
					}
				}
			}
		}

		output, err := bridge.runCommand(cmd, args)
		if err != nil {
			// Command ran but failed — return output with exit code.
			exitCode := 1
			errMsg := err.Error()
			// Parse exit code from error message if possible.
			if strings.HasPrefix(errMsg, "exit ") {
				fmt.Sscanf(errMsg, "exit %d:", &exitCode)
			}
			return rpcResponse{
				ID: req.ID,
				OK: true,
				Result: map[string]any{
					"output":    output,
					"exit_code": exitCode,
				},
			}
		}
		return rpcResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"output":    output,
				"exit_code": 0,
			},
		}

	default:
		return rpcResponse{
			ID: req.ID,
			OK: false,
			Error: &rpcError{
				Code:    "method_not_found",
				Message: fmt.Sprintf("unknown gt method %q", req.Method),
			},
		}
	}
}
