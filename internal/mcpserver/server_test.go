package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestServerInitializeListCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	srv := New(strings.NewReader(input), &out, nil)
	srv.Register(
		Tool{Name: "echo", Description: "echo", InputSchema: InputSchema{Type: "object"}},
		func(_ context.Context, args map[string]any) (*ToolCallResult, error) {
			text, _ := args["text"].(string)
			return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: text}}}, nil
		},
	)

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := splitNonEmpty(out.String())
	// initialize + tools/list + tools/call = 3 (notification produces no reply)
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d: %q", len(lines), out.String())
	}

	var initResp JSONRPCResponse
	mustJSON(t, lines[0], &initResp)
	res, _ := initResp.Result.(map[string]any)
	if res["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("bad protocol version: %v", res["protocolVersion"])
	}

	if !strings.Contains(lines[1], `"echo"`) {
		t.Fatalf("tools/list missing echo: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"hi"`) {
		t.Fatalf("tools/call missing echoed content: %s", lines[2])
	}
}

func TestServerUnknownMethod(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":9,"method":"does/not/exist"}` + "\n"
	var out bytes.Buffer
	srv := New(strings.NewReader(input), &out, nil)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(out.String(), "method not found") {
		t.Fatalf("expected method-not-found error, got %q", out.String())
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("unmarshal %q: %v", s, err)
	}
}
