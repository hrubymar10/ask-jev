package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/ask-jev/internal/jev"
)

func testConfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.jsonc")
	data, err := json.Marshal(map[string]any{"endpoint": endpoint, "api_key": "test-secret", "model": "jev-latest", "timeout_seconds": 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChoiceLogAndOutcome(t *testing.T) {
	var request jev.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing bearer authentication")
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"decision":{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":0.2},"confidence":0.6}},"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer server.Close()
	config := testConfig(t, server.URL)
	root := t.TempDir()
	state := filepath.Join(root, "state.json")
	if err := os.WriteFile(state, []byte(`{"case":"late"}`), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "decisions.jsonl")
	var stdout bytes.Buffer
	if err := run([]string{"choice", "--config", config, "--state", "@" + state, "--question", "Pick one", "--option", "a=first", "--option", "b", "--log", log}, &stdout); err != nil {
		t.Fatal(err)
	}
	var result output
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Primitive != "choice" || result.Answer == nil || result.Answer.Choice == nil || *result.Answer.Choice != "a" || result.RequestID == "" || result.RequestIDSource != "local" || result.Model != "jev-1.13.0" {
		t.Fatalf("invalid choice output: %s, %v", stdout.Bytes(), err)
	}
	var criteria map[string]*string
	data, _ := json.Marshal(request.Questions["decision"].Criteria)
	if err := json.Unmarshal(data, &criteria); err != nil || criteria["a"] == nil || *criteria["a"] != "first" || criteria["b"] != nil || string(request.State) != `{"case":"late"}` {
		t.Fatalf("invalid request criteria/state: %s, %v", data, err)
	}
	stdout.Reset()
	if err := run([]string{"outcome", result.RequestID, "true", "--log", log, "--config", config}, &stdout); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(log)
	if err != nil || bytes.Count(logged, []byte("\n")) != 2 || bytes.Contains(logged, []byte("test-secret")) || bytes.Contains(logged, []byte(`"case":"late"`)) || !bytes.Contains(logged, []byte(`"state_sha256":`)) || !bytes.Contains(logged, []byte(`"state_bytes":15`)) || !bytes.Contains(logged, []byte(`"value":true`)) {
		t.Fatalf("unsafe or incomplete log: %v", err)
	}
	info, err := os.Stat(log)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log permissions are not private: %v", err)
	}
	if err := run([]string{"outcome", result.RequestID, "false", "--log", log, "--config", config}, &stdout); err == nil {
		t.Fatal("duplicate outcome accepted")
	}
}

func TestOutcomeReadsLargeLoggedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	requestID := "large-record"
	state, err := json.Marshal(strings.Repeat("x", 5<<20))
	if err != nil {
		t.Fatal(err)
	}
	if err := appendLog(path, logEntry{Event: "evaluation", RequestID: requestID, State: state}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := recordOutcome(path, requestID, "true", &stdout); err != nil ||
		!bytes.Contains(stdout.Bytes(), []byte(`"value":true`)) {
		t.Fatalf("large outcome failed: %v", err)
	}
}

func TestNoulAndScoreRequestShapes(t *testing.T) {
	for _, tc := range []struct {
		kind, answer string
		args         []string
	}{
		{"noul", `{"type":"noul","noul":0.95}`, []string{"--true", "Yes", "--false", "No"}},
		{"score", `{"type":"score","score":1.2,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.2,"1":0.8},"confidence":0.6}`, []string{"--level", "low", "--level", "high"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var request jev.Request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"decision":` + tc.answer + `},"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer server.Close()
			args := append([]string{tc.kind, "--config", testConfig(t, server.URL), "--state", `"text"`, "--question", "Judge"}, tc.args...)
			var stdout bytes.Buffer
			if err := run(args, &stdout); err != nil {
				t.Fatal(err)
			}
			if request.Questions["decision"].Type != tc.kind || !strings.Contains(stdout.String(), `"primitive":"`+tc.kind+`"`) {
				t.Fatalf("wrong primitive response: %s", stdout.String())
			}
			encoded, _ := json.Marshal(request.Questions["decision"].Criteria)
			if tc.kind == "noul" && string(encoded) != `{"false":"No","true":"Yes"}` {
				t.Fatalf("wrong noul criteria: %s", encoded)
			}
			if tc.kind == "score" && string(encoded) != `["low","high"]` {
				t.Fatalf("wrong score criteria: %s", encoded)
			}
		})
	}
}

func TestBatchQuestionsAndExplicitStateLogging(t *testing.T) {
	var request jev.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.9},"route":{"type":"choice","choice":"a","probabilities":{"a":0.7,"b":0.3},"confidence":0.4}},"usage":{"input_tokens":7,"output_tokens":2,"cost":0.001},"cost":0.001}`))
	}))
	defer server.Close()
	root := t.TempDir()
	batch := filepath.Join(root, "batch.json")
	data := `{"state":{"private":"payload"},"questions":{"urgent":{"type":"noul","instructions":"Urgent?"},"route":{"type":"choice","instructions":{"question":"Route?"},"criteria":{"a":null,"b":null}}}}`
	if err := os.WriteFile(batch, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "batch.jsonl")
	var stdout bytes.Buffer
	if err := run([]string{"batch", "--config", testConfig(t, server.URL), "--file", batch, "--log", log, "--log-state"}, &stdout); err != nil {
		t.Fatal(err)
	}
	var result output
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Primitive != "batch" || len(result.Answers) != 2 || result.Answer != nil || request.Model != "jev-latest" || len(request.Questions) != 2 || string(result.Cost) != "0.001" || string(result.Usage.Cost) != "0.001" {
		t.Fatalf("invalid batch result or request: %s, %v", stdout.Bytes(), err)
	}
	logged, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(logged, []byte(`"state":{"private":"payload"}`)) || bytes.Contains(logged, []byte("test-secret")) {
		t.Fatalf("explicit state log mismatch: %v", err)
	}
}

func TestHumanOutput(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		args               []string
	}{
		{"choice", `{"type":"choice","choice":"billing","probabilities":{"billing":0.67,"technical":0.33},"confidence":0.4}`, "billing - Payments - 67% confidence\n", []string{"choice", "-H", "--option", "billing=Payments", "--option", "technical"}},
		{"noul yes", `{"type":"noul","noul":0.67}`, "YES - Human review - 67% confidence\n", []string{"noul", "--human", "--true", "Human review", "--false", "No review"}},
		{"noul no", `{"type":"noul","noul":0.36}`, "NO - No review - 64% confidence\n", []string{"noul", "-H", "--true", "Human review", "--false", "No review"}},
		{"noul no description", `{"type":"noul","noul":0.67}`, "YES - 67% confidence\n", []string{"noul", "-H"}},
		{"score", `{"type":"score","score":1.2,"legend":{"0":"low","1":"medium","2":"high"},"probabilities":{"0":0.1,"1":0.6,"2":0.3},"confidence":0.72}`, "1.2 - medium - 72% confidence\n", []string{"score", "-H", "--level", "low", "--level", "medium", "--level", "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model":"jev-test","answers":{"decision":` + tc.answer + `},"usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer server.Close()
			args := append(append([]string{}, tc.args...), "--config", testConfig(t, server.URL), "--state", `"ticket"`, "--question", "Decide")
			var stdout bytes.Buffer
			if err := run(args, &stdout); err != nil || stdout.String() != tc.want {
				t.Fatalf("human output=%q, error=%v", stdout.String(), err)
			}
		})
	}
}

func TestHumanBatchStillLogsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-test","answers":{"route":{"type":"choice","choice":"a","probabilities":{"a":0.7,"b":0.3},"confidence":0.2},"urgent":{"type":"noul","noul":0.4}},"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer server.Close()
	root := t.TempDir()
	batch := filepath.Join(root, "batch.json")
	if err := os.WriteFile(batch, []byte(`{"state":"ticket","questions":{"urgent":{"type":"noul","instructions":"Urgent?","criteria":{"false":"Can wait"}},"route":{"type":"choice","instructions":"Route?","criteria":{"a":"Billing","b":"Tech"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "decisions.jsonl")
	var stdout bytes.Buffer
	if err := run([]string{"batch", "-H", "--config", testConfig(t, server.URL), "--file", batch, "--log", log}, &stdout); err != nil {
		t.Fatal(err)
	}
	if want := "route: a - Billing - 70% confidence\nurgent: NO - Can wait - 60% confidence\n"; stdout.String() != want {
		t.Fatalf("human batch output=%q, want=%q", stdout.String(), want)
	}
	logged, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(logged, []byte(`"request_id"`)) || !bytes.Contains(logged, []byte(`"answers"`)) || bytes.Contains(logged, []byte(`"state":"ticket"`)) {
		t.Fatalf("JSONL evaluation missing or leaked state: %v", err)
	}
}

func TestHelpDoesNotLoadConfig(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"choice", "-h"}, &stdout); err != nil || !strings.Contains(stdout.String(), "-H or --human") {
		t.Fatalf("help output=%q, error=%v", stdout.String(), err)
	}
}
