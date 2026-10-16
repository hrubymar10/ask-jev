package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// Question is one documented System One primitive. Criteria has a different
// shape for each primitive: a map for choice or noul, and a slice for score.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type Request struct {
	State     json.RawMessage     `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        *string            `json:"choice,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

type Usage struct {
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	Cost         json.RawMessage `json:"cost,omitempty"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Cost    json.RawMessage   `json:"cost,omitempty"`
}

type Client struct {
	Endpoint string
	APIKey   string
	HTTP     *http.Client
}

func (c Client) Evaluate(ctx context.Context, request Request) (Response, error) {
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return Response{}, errors.New("endpoint must use HTTPS (HTTP is allowed only for loopback)")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return Response{}, errors.New("API key is required")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("encode request: %w", err)
	}
	if len(data) > 32<<20 {
		return Response{}, errors.New("request exceeds the API's 32 MiB body limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Response{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// net/http errors can include URLs and proxy details. Keep credentials and
		// request state out of terminal output and JSONL logs.
		return Response{}, errors.New("TypeSafe request failed (network or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("TypeSafe API returned HTTP %d", resp.StatusCode)
	}
	var result Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result); err != nil {
		return Response{}, errors.New("TypeSafe API returned invalid JSON")
	}
	if strings.TrimSpace(result.Model) == "" {
		return Response{}, errors.New("TypeSafe API omitted the model")
	}
	for name, question := range request.Questions {
		answer, ok := result.Answers[name]
		if !ok || answer.Type != question.Type {
			return Response{}, fmt.Errorf("TypeSafe API omitted or mismatched answer %q", name)
		}
		switch question.Type {
		case "choice":
			if answer.Choice == nil || answer.Probabilities == nil || answer.Confidence == nil {
				return Response{}, fmt.Errorf("TypeSafe API returned incomplete choice answer %q", name)
			}
		case "noul":
			if answer.Noul == nil {
				return Response{}, fmt.Errorf("TypeSafe API returned incomplete noul answer %q", name)
			}
		case "score":
			if answer.Score == nil || answer.Probabilities == nil || answer.Confidence == nil || answer.Legend == nil {
				return Response{}, fmt.Errorf("TypeSafe API returned incomplete score answer %q", name)
			}
		}
	}
	return result, nil
}

func validateRequest(request Request) error {
	if !json.Valid(request.State) {
		return errors.New("state must be valid JSON")
	}
	state := bytes.TrimSpace(request.State)
	if len(state) == 0 || (state[0] != '"' && state[0] != '{' && state[0] != '[') {
		return errors.New("state must be a JSON string, object, or array")
	}
	if strings.TrimSpace(request.Model) == "" {
		return errors.New("model is required")
	}
	if len(request.Questions) == 0 {
		return errors.New("at least one question is required")
	}
	for name, question := range request.Questions {
		if strings.TrimSpace(name) == "" || !validInstructions(question.Instructions) {
			return errors.New("question id and instructions are required")
		}
		if question.Type != "choice" && question.Type != "noul" && question.Type != "score" {
			return fmt.Errorf("unsupported question type %q", question.Type)
		}
	}
	return nil
}

func validInstructions(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	default:
		return false
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
