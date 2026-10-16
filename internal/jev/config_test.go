package jev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigStripsCommentsOutsideStrings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "key"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.jsonc")
	contents := `{
		// connection
		"endpoint": "http://127.0.0.1:1234/v1/systemone", /* local test */
		"api_key_path": "key",
		"model": "jev-latest",
		"timeout_seconds": 5,
		"log_path": "a//b.jsonl",
	}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil || config.Endpoint != "http://127.0.0.1:1234/v1/systemone" || config.APIKey != "secret" || config.Model != "jev-latest" || config.Timeout != 5*time.Second || config.LogPath != "a//b.jsonl" {
		t.Fatalf("config fields did not match: %v", err)
	}
	if _, err := stripJSONComments([]byte(`{"a":1, /* never ends`)); err == nil {
		t.Fatal("unterminated block comment accepted")
	}
}

func TestLoadConfigRejectsTwoKeySourcesWithoutPrintingThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.jsonc")
	if err := os.WriteFile(path, []byte(`{"api_key":"secret","api_key_path":"other","timeout_seconds":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe config error: %v", err)
	}
}
