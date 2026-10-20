package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	devicesv1 "github.com/codemowers/agent/operator/api/v1alpha1"
	authnv1 "k8s.io/api/authentication/v1"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

func tool(name, description string, properties map[string]any, required ...string) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}

func tools() []map[string]any {
	device := map[string]any{"type": "string", "description": "Name of a Device granted to this worker ServiceAccount"}
	path := map[string]any{"type": "string", "description": "Path on the remote device"}
	return []map[string]any{
		tool("list_devices", "List devices this worker holds any verb on, with those verbs", map[string]any{}),
		tool("device_shell", "Run a shell command on a device; requires the exec verb (30 second device timeout)", map[string]any{"device": device, "command": map[string]any{"type": "string"}}, "device", "command"),
		tool("device_file_read", "Read a file from a device; requires the read verb", map[string]any{"device": device, "path": path}, "device", "path"),
		tool("device_file_write", "Write a file on a device; requires the write verb", map[string]any{"device": device, "path": path, "content": map[string]any{"type": "string"}}, "device", "path", "content"),
	}
}

func (b *bridge) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	user, err := b.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var request mcpRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil || request.JSONRPC != "2.0" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if request.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	var rpcError any
	switch request.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "agent-devices", "title": "Devices", "version": "0.1.0"}, "instructions": b.instructions(r.Context(), user)}
	case "tools/list":
		result = map[string]any{"tools": tools()}
	case "tools/call":
		result = b.callTool(r.Context(), user, request.Params.Name, request.Params.Arguments)
	default:
		rpcError = map[string]any{"code": -32601, "message": "method not found"}
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	if rpcError != nil {
		response["error"] = rpcError
	} else {
		response["result"] = result
	}
	json.NewEncoder(w).Encode(response)
}

func mcpText(value string, failed bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": value}}, "isError": failed}
}

// instructions tells the model which remote Devices this caller may use, so
// that "devices" is not mistaken for the worker container's /dev.
func (b *bridge) instructions(ctx context.Context, user *authnv1.UserInfo) string {
	devices, err := b.devices(ctx)
	if err != nil {
		return "Remote device inventory is unavailable."
	}
	var parts []string
	for _, d := range devices {
		verbs := b.allowedVerbs(ctx, user, d.Name)
		if len(verbs) == 0 {
			continue
		}
		state := "disconnected"
		if d.Status.Connected {
			state = "connected"
			if d.Status.Hostname != "" {
				state += ", host " + d.Status.Hostname
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%s; may %s)", d.Name, state, strings.Join(verbs, ", ")))
	}
	if len(parts) == 0 {
		return "No remote devices are authorized for this worker."
	}
	return "Authorized remote devices: " + strings.Join(parts, "; ") +
		". These are physical machines, not the worker container's /dev. exec permits device_shell, read device_file_read and write device_file_write."
}

func (b *bridge) callTool(ctx context.Context, user *authnv1.UserInfo, name string, raw json.RawMessage) any {
	if name == "list_devices" {
		devices, err := b.devices(ctx)
		if err != nil {
			return mcpText("Device inventory unavailable", true)
		}
		visible := make([]map[string]any, 0)
		for _, d := range devices {
			if verbs := b.allowedVerbs(ctx, user, d.Name); len(verbs) > 0 {
				summary := deviceSummary(&d)
				summary["verbs"] = verbs
				visible = append(visible, summary)
			}
		}
		data, _ := json.Marshal(visible)
		return mcpText(string(data), false)
	}
	var args struct {
		Device  string `json:"device"`
		Command string `json:"command"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &args) != nil || args.Device == "" {
		return mcpText("Invalid device arguments", true)
	}
	verb, known := toolVerbs[name]
	if !known {
		return mcpText("Unknown tool", true)
	}
	allowed := b.authorized(ctx, user, args.Device, verb)
	log.Printf("device tool %s device=%s user=%s verb=%s allowed=%t", name, args.Device, user.Username, verb, allowed)
	if !allowed {
		return mcpText("Device "+verb+" access denied", true)
	}
	var command string
	var commandArgs any
	switch name {
	case "device_shell":
		if args.Command == "" || len(args.Command) > 8192 {
			return mcpText("Invalid command", true)
		}
		command, commandArgs = "shell", map[string]string{"command": args.Command}
	case "device_file_read":
		if args.Path == "" || len(args.Path) > 4096 {
			return mcpText("Invalid path", true)
		}
		command, commandArgs = "file_read", map[string]string{"path": args.Path}
	case "device_file_write":
		if args.Path == "" || len(args.Path) > 4096 || len(args.Content) > 1<<20 {
			return mcpText("Invalid file", true)
		}
		command, commandArgs = "file_write", map[string]string{"path": args.Path, "content": args.Content}
	default:
		return mcpText("Unknown tool", true)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	response, err := b.command(commandCtx, args.Device, command, commandArgs)
	if err != nil {
		return mcpText(err.Error(), true)
	}
	if !response.OK {
		return mcpText(limit(response.Error, 4096), true)
	}
	if name == "device_file_read" {
		return mcpText(limit(response.Content, 1<<20), false)
	}
	if name == "device_file_write" {
		return mcpText("File written", false)
	}
	return mcpText(fmt.Sprintf("exit_code: %d\nstdout:\n%s\nstderr:\n%s", response.ExitCode, response.Stdout, response.Stderr), false)
}

func deviceSummary(d *devicesv1.Device) map[string]any {
	return map[string]any{"name": d.Name, "displayName": d.Spec.DisplayName, "connected": d.Status.Connected,
		"hostname": d.Status.Hostname, "version": d.Status.Version, "lastSeenAt": d.Status.LastSeenAt}
}
