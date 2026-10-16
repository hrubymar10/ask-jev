package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/hrubymar10/ask-jev/internal/jev"
)

const mcpProtocolVersion = "2025-11-25"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func runMCP(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	defaultConfig := os.Getenv("ASK_JEV_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config.jsonc"
	}
	configPath := flags.String("config", defaultConfig, "config file")
	logPath := flags.String("log", "", "append JSONL path")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 {
		return errors.New("usage: ask-jev mcp [--config path] [--log path]")
	}
	config, err := jev.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *logPath == "" {
		*logPath = config.LogPath
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 34<<20)
	encoder := json.NewEncoder(stdout)
	initialized := false
	for scanner.Scan() {
		line := scanner.Bytes()
		var request mcpRequest
		if err := json.Unmarshal(line, &request); err != nil {
			rpcError := &mcpRPCError{-32700, "Parse error"}
			if json.Valid(line) {
				rpcError = &mcpRPCError{-32600, "Invalid Request"}
			}
			if err := encoder.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: rpcError}); err != nil {
				return err
			}
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
			if err := encoder.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &mcpRPCError{-32600, "Invalid Request"}}); err != nil {
				return err
			}
			continue
		}
		id := request.ID
		if len(id) == 0 || string(id) == "null" {
			if request.JSONRPC == "2.0" && request.Method == "notifications/initialized" {
				initialized = true
			}
			continue
		}
		if !validMCPID(id) || request.JSONRPC != "2.0" || request.Method == "" {
			id = json.RawMessage("null")
			if err := encoder.Encode(mcpResponse{JSONRPC: "2.0", ID: id,
				Error: &mcpRPCError{-32600, "Invalid Request"}}); err != nil {
				return err
			}
			continue
		}
		reply := mcpResponse{JSONRPC: "2.0", ID: id}
		switch request.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.ProtocolVersion == "" {
				reply.Error = &mcpRPCError{-32602, "Invalid params"}
			} else {
				negotiated := mcpProtocolVersion
				switch params.ProtocolVersion {
				case "2024-11-05", "2025-03-26", "2025-06-18", mcpProtocolVersion:
					negotiated = params.ProtocolVersion
				}
				reply.Result = map[string]any{
					"protocolVersion": negotiated,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "ask-jev", "version": version},
				}
			}
		case "ping":
			reply.Result = map[string]any{}
		case "tools/list":
			if !initialized {
				reply.Error = &mcpRPCError{-32000, "Server is not initialized"}
			} else {
				reply.Result = map[string]any{"tools": mcpTools()}
			}
		case "tools/call":
			if !initialized {
				reply.Error = &mcpRPCError{-32000, "Server is not initialized"}
				break
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.Name == "" {
				reply.Error = &mcpRPCError{-32602, "Invalid params"}
				break
			}
			if !knownMCPTool(params.Name) {
				reply.Error = &mcpRPCError{-32602, "Unknown tool"}
				break
			}
			data, callErr := callMCPTool(config, *logPath, params.Name, params.Arguments)
			result := map[string]any{"content": []map[string]string{{"type": "text", "text": string(data)}}}
			if callErr != nil {
				result["content"] = []map[string]string{{"type": "text", "text": safeMCPError(callErr, config.APIKey, params.Arguments)}}
				result["isError"] = true
			}
			reply.Result = result
		default:
			reply.Error = &mcpRPCError{-32601, "Method not found"}
		}
		if err := encoder.Encode(reply); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func validMCPID(id json.RawMessage) bool {
	var value any
	if json.Unmarshal(id, &value) != nil {
		return false
	}
	switch value.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}

func knownMCPTool(name string) bool {
	switch name {
	case "choice", "noul", "score", "batch", "outcome":
		return true
	}
	return false
}

func mcpTools() []mcpTool {
	state := map[string]any{"oneOf": []any{
		map[string]any{"type": "string"}, map[string]any{"type": "object"}, map[string]any{"type": "array"},
	}}
	base := map[string]any{"state": state, "question": map[string]any{"type": "string", "minLength": 1}}
	copyBase := func() map[string]any {
		properties := make(map[string]any, len(base)+1)
		for key, value := range base {
			properties[key] = value
		}
		return properties
	}
	schema := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required,
			"additionalProperties": false}
	}
	choice := copyBase()
	choice["options"] = map[string]any{"type": "object", "minProperties": 2, "maxProperties": 255,
		"additionalProperties": map[string]any{"type": []string{"string", "null"}}}
	noul := copyBase()
	noul["true"] = map[string]any{"type": "string"}
	noul["false"] = map[string]any{"type": "string"}
	score := copyBase()
	score["levels"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1},
		"minItems": 2, "maxItems": 10}
	question := schema(map[string]any{
		"type": map[string]any{"type": "string", "enum": []string{"choice", "noul", "score"}},
		"instructions": map[string]any{"oneOf": []any{
			map[string]any{"type": "string"}, map[string]any{"type": "object"}, map[string]any{"type": "array"}}},
		"criteria": map[string]any{"oneOf": []any{
			map[string]any{"type": "object"}, map[string]any{"type": "array"}}},
	}, "type", "instructions")
	return []mcpTool{
		{"choice", "Choose one option for a question.", schema(choice, "state", "question", "options")},
		{"noul", "Estimate the probability that the answer is yes.", schema(noul, "state", "question")},
		{"score", "Score a question against ordered levels.", schema(score, "state", "question", "levels")},
		{"batch", "Evaluate several questions in one request.", schema(map[string]any{
			"state": state, "questions": map[string]any{"type": "object", "minProperties": 1,
				"additionalProperties": question}}, "state", "questions")},
		{"outcome", "Record an observed outcome in the local JSONL log.", schema(map[string]any{
			"request_id": map[string]any{"type": "string", "minLength": 1},
			"value":      map[string]any{}}, "request_id", "value")},
	}
}

func callMCPTool(config jev.Config, logPath, name string, arguments json.RawMessage) ([]byte, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	if name == "outcome" {
		var args struct {
			RequestID string          `json:"request_id"`
			Value     json.RawMessage `json:"value"`
		}
		if !decodeMCPArgs(arguments, &args) || strings.TrimSpace(args.RequestID) == "" || len(args.Value) == 0 {
			return nil, errors.New("outcome needs request_id and value")
		}
		if logPath == "" {
			return nil, errors.New("outcome needs --log or config log_path")
		}
		var result bytes.Buffer
		if err := recordOutcome(logPath, args.RequestID, string(args.Value), &result); err != nil {
			return nil, err
		}
		return bytes.TrimSpace(result.Bytes()), nil
	}
	var request jev.Request
	if name == "batch" {
		var args struct {
			State     json.RawMessage         `json:"state"`
			Questions map[string]jev.Question `json:"questions"`
		}
		if !decodeMCPArgs(arguments, &args) || len(args.State) == 0 || len(args.Questions) == 0 {
			return nil, errors.New("batch needs state and questions")
		}
		request = jev.Request{State: args.State, Model: config.Model, Questions: args.Questions}
	} else {
		var args struct {
			State    json.RawMessage    `json:"state"`
			Question string             `json:"question"`
			Options  map[string]*string `json:"options"`
			True     string             `json:"true"`
			False    string             `json:"false"`
			Levels   []string           `json:"levels"`
		}
		if !decodeMCPArgs(arguments, &args) || len(args.State) == 0 || strings.TrimSpace(args.Question) == "" {
			return nil, errors.New("tool needs state and question")
		}
		q := jev.Question{Type: name, Instructions: args.Question}
		switch name {
		case "choice":
			if len(args.Options) < 2 || len(args.Options) > 255 {
				return nil, errors.New("choice needs 2 to 255 options")
			}
			for key := range args.Options {
				if strings.TrimSpace(key) == "" {
					return nil, errors.New("choice option names must be nonempty")
				}
			}
			q.Criteria = args.Options
		case "noul":
			if args.True != "" || args.False != "" {
				criteria := map[string]string{}
				if args.True != "" {
					criteria["true"] = args.True
				}
				if args.False != "" {
					criteria["false"] = args.False
				}
				q.Criteria = criteria
			}
		case "score":
			if len(args.Levels) < 2 || len(args.Levels) > 10 {
				return nil, errors.New("score needs 2 to 10 levels")
			}
			for _, level := range args.Levels {
				if strings.TrimSpace(level) == "" {
					return nil, errors.New("score levels must be nonempty")
				}
			}
			q.Criteria = args.Levels
		}
		request = jev.Request{State: args.State, Model: config.Model,
			Questions: map[string]jev.Question{"decision": q}}
	}
	requestID, err := newRequestID()
	if err != nil {
		return nil, errors.New("could not generate request id")
	}
	client := jev.Client{Endpoint: config.Endpoint, APIKey: config.APIKey,
		HTTP: &http.Client{Timeout: config.Timeout}}
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()
	start := time.Now()
	response, err := client.Evaluate(ctx, request)
	if err != nil {
		return nil, err
	}
	result := output{RequestID: requestID, RequestIDSource: "local", Primitive: name,
		Model: response.Model, LatencyMS: time.Since(start).Milliseconds(),
		Usage: response.Usage, Cost: response.Cost}
	if name == "batch" {
		result.Answers = response.Answers
	} else {
		answer := response.Answers["decision"]
		result.Answer = &answer
	}
	if logPath != "" {
		entry := logEntry{Event: "evaluation", Time: time.Now().UTC(), RequestID: requestID,
			StateHash: fmt.Sprintf("%x", sha256.Sum256(request.State)),
			StateSize: len(request.State), Output: &result}
		if err := appendLog(logPath, entry); err != nil {
			return nil, err
		}
	}
	return json.Marshal(result)
}

func decodeMCPArgs(data json.RawMessage, dst any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst) == nil && decoder.Decode(new(any)) == io.EOF
}

func safeMCPError(err error, key string, arguments json.RawMessage) string {
	message := err.Error()
	var args map[string]json.RawMessage
	_ = json.Unmarshal(arguments, &args)
	var stateString string
	_ = json.Unmarshal(args["state"], &stateString)
	for _, private := range []string{key, string(args["state"]), stateString, string(arguments)} {
		if private != "" && strings.Contains(message, private) {
			return "tool call failed"
		}
	}
	message = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > 300 {
		message = string(runes[:300])
	}
	return message
}
