package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Endpoint string
	APIKey   string
	Model    string
	Timeout  time.Duration
	LogPath  string
}

type configFile struct {
	Endpoint       string `json:"endpoint"`
	APIKey         string `json:"api_key"`
	APIKeyPath     string `json:"api_key_path"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	LogPath        string `json:"log_path"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	clean, err := stripJSONComments(data)
	if err != nil {
		return Config{}, err
	}
	var file configFile
	decoder := json.NewDecoder(bytes.NewReader(clean))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("config contains more than one JSON value")
		}
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if file.APIKey != "" && file.APIKeyPath != "" {
		return Config{}, errors.New("configure api_key or api_key_path, not both")
	}
	if file.APIKeyPath != "" {
		keyPath := file.APIKeyPath
		if !filepath.IsAbs(keyPath) {
			keyPath = filepath.Join(filepath.Dir(path), keyPath)
		}
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return Config{}, errors.New("could not read API key file")
		}
		file.APIKey = strings.TrimSpace(string(key))
	}
	if file.TimeoutSeconds <= 0 {
		return Config{}, errors.New("timeout_seconds must be positive")
	}
	if file.Endpoint == "" {
		file.Endpoint = DefaultEndpoint
	}
	return Config{Endpoint: file.Endpoint, APIKey: strings.TrimSpace(file.APIKey), Model: file.Model,
		Timeout: time.Duration(file.TimeoutSeconds) * time.Second, LogPath: file.LogPath}, nil
}

// stripJSONComments removes JavaScript line and block comments without
// altering quoted strings. Newlines remain so JSON errors retain useful lines.
func stripJSONComments(input []byte) ([]byte, error) {
	var out bytes.Buffer
	inString, escaped, lineComment, blockComment := false, false, false, false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		switch {
		case lineComment:
			if ch == '\n' {
				lineComment = false
				out.WriteByte(ch)
			}
		case blockComment:
			if ch == '*' && i+1 < len(input) && input[i+1] == '/' {
				blockComment = false
				i++
			} else if ch == '\n' {
				out.WriteByte(ch)
			}
		case inString:
			out.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
		case ch == '"':
			inString = true
			out.WriteByte(ch)
		case ch == '/' && i+1 < len(input) && input[i+1] == '/':
			lineComment = true
			i++
		case ch == '/' && i+1 < len(input) && input[i+1] == '*':
			blockComment = true
			i++
		default:
			out.WriteByte(ch)
		}
	}
	if blockComment {
		return nil, errors.New("config has an unterminated block comment")
	}
	return stripTrailingCommas(out.Bytes()), nil
}

func stripTrailingCommas(input []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if inString {
			out.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(input) && (input[j] == ' ' || input[j] == '\t' || input[j] == '\r' || input[j] == '\n') {
				j++
			}
			if j < len(input) && (input[j] == '}' || input[j] == ']') {
				continue
			}
		}
		out.WriteByte(ch)
	}
	return out.Bytes()
}
