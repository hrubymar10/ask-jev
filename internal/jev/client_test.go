package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvaluateThreePrimitives(t *testing.T) {
	answers := map[string]string{
		"choice": `{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":0.2},"confidence":0.6}`,
		"noul":   `{"type":"noul","noul":0.9}`,
		"score":  `{"type":"score","score":1.2,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"0":0,"1":0.8,"2":0.2},"confidence":0.7}`,
	}
	for kind, answer := range answers {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request method/path/headers")
				}
				var body struct {
					State     json.RawMessage     `json:"state"`
					Model     string              `json:"model"`
					Questions map[string]Question `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body.State) != `{"ticket":"late"}` || body.Model != "jev-latest" || body.Questions["decision"].Type != kind {
					t.Errorf("request body=%+v error=%v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"decision":` + answer + `},"usage":{"input_tokens":12,"output_tokens":4}}`))
			}))
			defer server.Close()
			client := Client{Endpoint: server.URL + "/v1/systemone", APIKey: "secret"}
			response, err := client.Evaluate(context.Background(), Request{State: json.RawMessage(`{"ticket":"late"}`), Model: "jev-latest",
				Questions: map[string]Question{"decision": {Type: kind, Instructions: "Decide"}}})
			if err != nil || response.Answers["decision"].Type != kind || response.Usage.InputTokens != 12 {
				t.Fatalf("response=%+v error=%v", response, err)
			}
		})
	}
}

func TestEvaluateErrorsNeverExposeKeyOrBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"secret and private state"}`))
	}))
	defer server.Close()
	client := Client{Endpoint: server.URL, APIKey: "secret"}
	_, err := client.Evaluate(context.Background(), Request{State: json.RawMessage(`"private state"`), Model: "jev-latest",
		Questions: map[string]Question{"decision": {Type: "noul", Instructions: "Yes?"}}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private state") {
		t.Fatalf("unsafe error: %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}))
	defer slow.Close()
	client.Endpoint = slow.URL
	client.HTTP = &http.Client{Timeout: time.Millisecond}
	_, err = client.Evaluate(context.Background(), Request{State: json.RawMessage(`"private state"`), Model: "jev-latest",
		Questions: map[string]Question{"decision": {Type: "noul", Instructions: "Yes?"}}})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private state") {
		t.Fatalf("unsafe timeout error: %v", err)
	}
}

func TestEvaluateShowsBoundedAPIErrorReason(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"nested message", `{"error":{"message":"invalid criteria"}}`, "invalid criteria"},
		{"plain body", "rate limit reached", "rate limit reached"},
		{"long body", strings.Repeat("x", 400), strings.Repeat("x", 300) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := Client{Endpoint: server.URL, APIKey: "test-secret"}
			_, err := client.Evaluate(context.Background(), Request{State: json.RawMessage(`"private state"`), Model: "jev-latest",
				Questions: map[string]Question{"decision": {Type: "noul", Instructions: "Yes?"}}})
			if err == nil || err.Error() != "TypeSafe API returned HTTP 400: "+tc.want {
				t.Fatalf("unexpected API error: %v", err)
			}
		})
	}
}
