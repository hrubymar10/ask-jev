package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/ask-jev/internal/jev"
)

func mcpExchange(t *testing.T, config, logPath string, requests ...any) []map[string]json.RawMessage {
	t.Helper()
	var input, output bytes.Buffer
	for _, request := range requests {
		switch value := request.(type) {
		case string:
			input.WriteString(value)
		default:
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			input.Write(data)
		}
		input.WriteByte('\n')
	}
	args := []string{"--config", config}
	if logPath != "" {
		args = append(args, "--log", logPath)
	}
	if err := runMCP(args, &input, &output); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]json.RawMessage
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("non-JSON-RPC stdout: %q: %v", scanner.Bytes(), err)
		}
		responses = append(responses, response)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return responses
}

func mcpInitialize(version string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": version,
			"capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}}
}

func mcpCall(id int, name string, args any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}}
}

func mcpResult(t *testing.T, response map[string]json.RawMessage) (string, bool) {
	t.Helper()
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil ||
		len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("bad tool result: %s: %v", response["result"], err)
	}
	return result.Content[0].Text, result.IsError
}

func TestMCPHandshakeAndProtocolErrors(t *testing.T) {
	config := testConfig(t, "http://127.0.0.1:1")
	responses := mcpExchange(t, config, "",
		mcpInitialize("2025-06-18"),
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"},
		"{",
		map[string]any{"jsonrpc": "2.0", "id": 4, "method": "unsupported"},
		mcpCall(5, "missing", map[string]any{}),
		map[string]any{"jsonrpc": "1.0", "id": 6, "method": "ping"},
	)
	if len(responses) != 7 {
		t.Fatalf("got %d responses, want 7", len(responses))
	}
	if !bytes.Contains(responses[0]["result"], []byte(`"protocolVersion":"2025-06-18"`)) ||
		!bytes.Contains(responses[0]["result"], []byte(`"name":"ask-jev"`)) {
		t.Fatalf("bad initialize: %s", responses[0]["result"])
	}
	var listed struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(responses[1]["result"], &listed); err != nil || len(listed.Tools) != 5 {
		t.Fatalf("bad tools list: %s: %v", responses[1]["result"], err)
	}
	for _, tool := range listed.Tools {
		if tool.InputSchema["type"] != "object" || tool.InputSchema["properties"] == nil ||
			tool.InputSchema["required"] == nil {
			t.Fatalf("bad schema for %s: %#v", tool.Name, tool.InputSchema)
		}
	}
	if string(responses[2]["result"]) != "{}" {
		t.Fatalf("bad ping: %s", responses[2]["result"])
	}
	for index, code := range []string{"-32700", "-32601", "-32602", "-32600"} {
		if !bytes.Contains(responses[index+3]["error"], []byte(code)) {
			t.Fatalf("wrong error at %d: %s", index, responses[index+3]["error"])
		}
	}
	fallback := mcpExchange(t, config, "", mcpInitialize("2026-07-28"))
	if !bytes.Contains(fallback[0]["result"], []byte(`"protocolVersion":"2025-11-25"`)) {
		t.Fatalf("bad version fallback: %s", fallback[0]["result"])
	}
}

func TestMCPToolsAndOutcome(t *testing.T) {
	var types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing bearer token")
		}
		var request jev.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Model != "jev-latest" || string(request.State) != `{"private":"payload"}` {
			t.Errorf("wrong model or state: %s", request.Model)
		}
		for id, question := range request.Questions {
			types = append(types, question.Type)
			answer := ""
			switch question.Type {
			case "choice":
				answer = `{"type":"choice","choice":"a","probabilities":{"a":0.7,"b":0.3},"confidence":0.5}`
			case "noul":
				answer = `{"type":"noul","noul":0.8}`
			case "score":
				answer = `{"type":"score","score":1.2,"probabilities":{"0":0.2,"1":0.8},"legend":{"0":"low","1":"high"},"confidence":0.6}`
			}
			_, _ = fmt.Fprintf(w, `{"model":"jev-test","answers":{%q:%s},"usage":{"input_tokens":1,"output_tokens":2}}`, id, answer)
			return
		}
	}))
	defer server.Close()
	config := testConfig(t, server.URL)
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	state := map[string]any{"private": "payload"}
	responses := mcpExchange(t, config, logPath, mcpInitialize(mcpProtocolVersion),
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		mcpCall(2, "choice", map[string]any{"state": state, "question": "Choose", "options": map[string]any{"a": "First", "b": nil}}),
		mcpCall(3, "noul", map[string]any{"state": state, "question": "Yes?", "true": "Yes", "false": "No"}),
		mcpCall(4, "score", map[string]any{"state": state, "question": "Level?", "levels": []string{"low", "high"}}),
		mcpCall(5, "batch", map[string]any{"state": state, "questions": map[string]any{
			"test": map[string]any{"type": "noul", "instructions": "Yes?"}}}),
	)
	if len(responses) != 5 || strings.Join(types, ",") != "choice,noul,score,noul" {
		t.Fatalf("wrong calls: %v, %d replies", types, len(responses))
	}
	var first output
	for i, kind := range []string{"choice", "noul", "score", "batch"} {
		text, isError := mcpResult(t, responses[i+1])
		var result output
		if isError || json.Unmarshal([]byte(text), &result) != nil || result.Primitive != kind ||
			result.RequestID == "" {
			t.Fatalf("bad %s result: %s", kind, text)
		}
		if i == 0 {
			first = result
		}
	}
	outcome := mcpExchange(t, config, logPath, mcpInitialize(mcpProtocolVersion),
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		mcpCall(2, "outcome", map[string]any{"request_id": first.RequestID, "value": true}))
	text, isError := mcpResult(t, outcome[1])
	if isError || !strings.Contains(text, `"value":true`) {
		t.Fatalf("bad outcome: %s", text)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || bytes.Count(logged, []byte("\n")) != 5 ||
		bytes.Contains(logged, []byte("test-secret")) ||
		bytes.Contains(logged, []byte("payload")) {
		t.Fatalf("unsafe or incomplete JSONL: %v", err)
	}
}

func TestMCPAPIErrorsAreToolErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"test-secret and payload"}}`))
	}))
	defer server.Close()
	responses := mcpExchange(t, testConfig(t, server.URL), "", mcpInitialize(mcpProtocolVersion),
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		mcpCall(2, "noul", map[string]any{"state": "payload", "question": "Yes?"}))
	text, isError := mcpResult(t, responses[1])
	if !isError || strings.Contains(text, "test-secret") || strings.Contains(text, "payload") {
		t.Fatalf("unsafe tool error: %q", text)
	}
}
