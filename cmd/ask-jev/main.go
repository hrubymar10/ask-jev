package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hrubymar10/ask-jev/internal/jev"
)

var version = "dev"

type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }
func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}

type output struct {
	RequestID       string                `json:"request_id"`
	RequestIDSource string                `json:"request_id_source"`
	Primitive       string                `json:"primitive"`
	Answer          *jev.Answer           `json:"answer,omitempty"`
	Answers         map[string]jev.Answer `json:"answers,omitempty"`
	Model           string                `json:"model"`
	LatencyMS       int64                 `json:"latency_ms"`
	Usage           jev.Usage             `json:"usage"`
	Cost            json.RawMessage       `json:"cost,omitempty"`
}

type logEntry struct {
	Event     string          `json:"event"`
	Time      time.Time       `json:"time"`
	RequestID string          `json:"request_id"`
	StateHash string          `json:"state_sha256,omitempty"`
	StateSize int             `json:"state_bytes,omitempty"`
	State     json.RawMessage `json:"state,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Output    *output         `json:"output,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ask-jev:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(stdout, usage)
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: ask-jev <choice|noul|score|batch|outcome|version> [flags]")
	}
	command := args[0]
	if command == "version" {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	if command != "choice" && command != "noul" && command != "score" && command != "batch" && command != "outcome" {
		return fmt.Errorf("unknown command %q", command)
	}
	ordered, err := flagsFirst(args[1:])
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	help := flags.Bool("help", false, "show usage")
	shortHelp := flags.Bool("h", false, "show usage")
	human := flags.Bool("human", false, "print a readable answer")
	shortHuman := flags.Bool("H", false, "print a readable answer")
	defaultConfig := os.Getenv("ASK_JEV_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config.jsonc"
	}
	configPath := flags.String("config", defaultConfig, "config file")
	logPath := flags.String("log", "", "append JSONL path")
	logState := flags.Bool("log-state", false, "include full state in JSONL")
	model := flags.String("model", "", "model override")
	timeout := flags.String("timeout", "", "timeout override, e.g. 15s")
	filePath := flags.String("file", "", "batch request JSON file")
	state := flags.String("state", "", "JSON state or @file")
	question := flags.String("question", "", "question text")
	trueDescription := flags.String("true", "", "description of yes")
	falseDescription := flags.String("false", "", "description of no")
	var options, levels repeated
	flags.Var(&options, "option", "choice option key[=description]; repeat")
	flags.Var(&levels, "level", "score level description; repeat")
	if err := flags.Parse(ordered); err != nil {
		return errors.New("invalid flags; see README for command syntax")
	}
	if *help || *shortHelp {
		_, err := fmt.Fprintln(stdout, usage)
		return err
	}
	if command == "outcome" && (*human || *shortHuman) {
		return errors.New("--human applies only to evaluations")
	}
	config, err := jev.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *logPath == "" {
		*logPath = config.LogPath
	}
	if *logState && *logPath == "" {
		return errors.New("--log-state requires --log or config log_path")
	}
	if command == "outcome" {
		if *logState {
			return errors.New("--log-state applies only to evaluations")
		}
		if len(flags.Args()) != 2 {
			return errors.New("usage: ask-jev outcome <request_id> <value> --log <jsonl>")
		}
		if *logPath == "" {
			return errors.New("outcome needs --log or config log_path")
		}
		return recordOutcome(*logPath, flags.Arg(0), flags.Arg(1), stdout)
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *model != "" {
		config.Model = *model
	}
	if *timeout != "" {
		config.Timeout, err = time.ParseDuration(*timeout)
		if err != nil || config.Timeout <= 0 {
			return errors.New("--timeout must be a positive duration such as 15s")
		}
	}
	var request jev.Request
	if command == "batch" {
		if *state != "" || *question != "" || len(options) != 0 || len(levels) != 0 || *trueDescription != "" || *falseDescription != "" {
			return errors.New("batch reads state and questions from --file")
		}
		if *filePath == "" {
			return errors.New("batch needs --file with a JSON request")
		}
		data, err := os.ReadFile(*filePath)
		if err != nil {
			return fmt.Errorf("read batch file: %w", err)
		}
		if len(data) > 32<<20 {
			return errors.New("batch file exceeds the API's 32 MiB request limit")
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return errors.New("batch file must contain a JSON System One request")
		}
		if *model != "" {
			request.Model = *model
		} else if request.Model == "" {
			request.Model = config.Model
		}
	} else {
		if *filePath != "" {
			return errors.New("--file applies only to batch")
		}
		if *state == "" || strings.TrimSpace(*question) == "" {
			return errors.New("--state and --question are required")
		}
		stateJSON, err := readState(*state)
		if err != nil {
			return err
		}
		primitive, err := buildQuestion(command, *question, options, levels, *trueDescription, *falseDescription)
		if err != nil {
			return err
		}
		request = jev.Request{State: stateJSON, Model: config.Model,
			Questions: map[string]jev.Question{"decision": primitive}}
	}
	requestID, err := newRequestID()
	if err != nil {
		return errors.New("could not generate request id")
	}
	client := jev.Client{Endpoint: config.Endpoint, APIKey: config.APIKey,
		HTTP: &http.Client{Timeout: config.Timeout}}
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()
	start := time.Now()
	response, err := client.Evaluate(ctx, request)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return err
	}
	result := output{RequestID: requestID, RequestIDSource: "local", Primitive: command,
		Model: response.Model, LatencyMS: latency, Usage: response.Usage, Cost: response.Cost}
	if command == "batch" {
		result.Answers = response.Answers
	} else {
		answer := response.Answers["decision"]
		result.Answer = &answer
	}
	if *logPath != "" {
		entry := logEntry{Event: "evaluation", Time: time.Now().UTC(), RequestID: requestID,
			StateHash: fmt.Sprintf("%x", sha256.Sum256(request.State)), StateSize: len(request.State), Output: &result}
		if *logState {
			entry.State = request.State
		}
		if err := appendLog(*logPath, entry); err != nil {
			return err
		}
	}
	if *human || *shortHuman {
		return printHuman(stdout, command == "batch", request, response)
	}
	return json.NewEncoder(stdout).Encode(result)
}

const usage = "usage: ask-jev <choice|noul|score|batch|outcome|version> [flags]\n" +
	"Use -H or --human for readable evaluation output; use -h or --help for this help."

func printHuman(dst io.Writer, batch bool, request jev.Request, response jev.Response) error {
	ids := make([]string, 0, len(request.Questions))
	for id := range request.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		line := humanAnswer(request.Questions[id], response.Answers[id])
		if batch {
			line = oneLine(id) + ": " + line
		}
		if _, err := fmt.Fprintln(dst, line); err != nil {
			return err
		}
	}
	return nil
}

func humanAnswer(question jev.Question, answer jev.Answer) string {
	var label, description string
	var confidence float64
	switch answer.Type {
	case "choice":
		label = *answer.Choice
		description = criterionDescription(question.Criteria, label)
		confidence = answer.Probabilities[label]
	case "noul":
		probability := *answer.Noul
		label = "NO"
		criterion := "false"
		confidence = 1 - probability
		if probability >= 0.5 {
			label, criterion, confidence = "YES", "true", probability
		}
		description = criterionDescription(question.Criteria, criterion)
	case "score":
		value := *answer.Score
		label = strconv.FormatFloat(value, 'f', -1, 64)
		index := int(math.Round(value))
		if index >= 0 {
			description = answer.Legend[strconv.Itoa(index)]
			if description == "" {
				description = scoreLevel(question.Criteria, index)
			}
		}
		confidence = *answer.Confidence
	}
	line := oneLine(label)
	if description != "" {
		line += " - " + oneLine(description)
	}
	return fmt.Sprintf("%s - %.0f%% confidence", line, confidence*100)
}

func criterionDescription(criteria any, key string) string {
	switch values := criteria.(type) {
	case map[string]*string:
		if value := values[key]; value != nil {
			return *value
		}
	case map[string]string:
		return values[key]
	case map[string]any:
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}

func scoreLevel(criteria any, index int) string {
	switch values := criteria.(type) {
	case []string:
		if index < len(values) {
			return values[index]
		}
	case []any:
		if index < len(values) {
			if value, ok := values[index].(string); ok {
				return value
			}
		}
	}
	return ""
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

// The standard flag parser stops at the first positional argument. Move
// known value flags ahead of positionals so outcome accepts --log afterward.
func flagsFirst(args []string) ([]string, error) {
	var values, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") && arg != "-h" && arg != "-H" {
			positionals = append(positionals, arg)
			continue
		}
		name, _, hasValue := strings.Cut(arg, "=")
		if name == "--log-state" || name == "--human" || name == "-H" || name == "-h" || name == "--help" {
			values = append(values, arg)
			continue
		}
		switch name {
		case "--config", "--log", "--model", "--timeout", "--file", "--state", "--question", "--option", "--level", "--true", "--false":
		default:
			return nil, fmt.Errorf("unknown flag %q", name)
		}
		if hasValue {
			values = append(values, arg)
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s needs a value", name)
		}
		values = append(values, arg, args[i+1])
		i++
	}
	return append(values, positionals...), nil
}

func readState(value string) (json.RawMessage, error) {
	var data []byte
	if strings.HasPrefix(value, "@") {
		if len(value) == 1 {
			return nil, errors.New("--state @ needs a file path")
		}
		var err error
		data, err = os.ReadFile(value[1:])
		if err != nil {
			return nil, fmt.Errorf("read state file: %w", err)
		}
	} else {
		data = []byte(value)
	}
	if len(data) > 32<<20 {
		return nil, errors.New("state exceeds the API's 32 MiB request limit")
	}
	if !json.Valid(data) {
		return nil, errors.New("state must be valid JSON")
	}
	return data, nil
}

func buildQuestion(kind, text string, options, levels []string, yes, no string) (jev.Question, error) {
	question := jev.Question{Type: kind, Instructions: text}
	switch kind {
	case "choice":
		if len(levels) != 0 || yes != "" || no != "" {
			return question, errors.New("choice accepts --option, not --level, --true, or --false")
		}
		if len(options) < 2 || len(options) > 255 {
			return question, errors.New("choice needs 2 to 255 --option values")
		}
		criteria := make(map[string]*string, len(options))
		for _, option := range options {
			key, description, specified := strings.Cut(option, "=")
			if strings.TrimSpace(key) == "" {
				return question, errors.New("choice option name cannot be empty")
			}
			if _, exists := criteria[key]; exists {
				return question, fmt.Errorf("duplicate choice option %q", key)
			}
			if specified {
				criteria[key] = &description
			} else {
				criteria[key] = nil
			}
		}
		question.Criteria = criteria
	case "noul":
		if len(options) != 0 || len(levels) != 0 {
			return question, errors.New("noul accepts --true and --false, not --option or --level")
		}
		if yes != "" || no != "" {
			criteria := map[string]string{}
			if yes != "" {
				criteria["true"] = yes
			}
			if no != "" {
				criteria["false"] = no
			}
			question.Criteria = criteria
		}
	case "score":
		if len(options) != 0 || yes != "" || no != "" {
			return question, errors.New("score accepts --level, not --option, --true, or --false")
		}
		if len(levels) < 2 || len(levels) > 10 {
			return question, errors.New("score needs 2 to 10 --level values")
		}
		for _, level := range levels {
			if strings.TrimSpace(level) == "" {
				return question, errors.New("score level cannot be empty")
			}
		}
		question.Criteria = levels
	}
	return question, nil
}

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func appendLog(path string, entry logEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.New("could not encode log entry")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	return file.Sync()
}

func recordOutcome(path, requestID, value string, stdout io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	found, alreadyRecorded := false, false
	for scanner.Scan() {
		var entry logEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			return errors.New("log contains an invalid JSONL entry")
		}
		if entry.RequestID == requestID {
			found = found || entry.Event == "evaluation"
			alreadyRecorded = alreadyRecorded || entry.Event == "outcome"
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	if !found {
		return errors.New("request id was not found in the log")
	}
	if alreadyRecorded {
		return errors.New("outcome already recorded for this request id")
	}
	actual := json.RawMessage(value)
	if !json.Valid(actual) {
		actual, _ = json.Marshal(value)
	}
	entry := logEntry{Event: "outcome", Time: time.Now().UTC(), RequestID: requestID, Value: actual}
	if err := appendLog(path, entry); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(entry)
}
