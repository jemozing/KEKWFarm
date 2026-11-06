package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRunnerToken(t *testing.T) {
	localToken := "kr_" + strings.Repeat("a", 64)
	envToken := "kr_" + strings.Repeat("b", 64)
	dir := t.TempDir()
	t.Setenv("KEKWFARM_RUNNER_TOKEN", "")
	if token, err := loadToken("", dir); err != nil || token != "" {
		t.Fatalf("missing optional token should allow legacy authentication, error: %v", err)
	}
	defaultPath := filepath.Join(dir, "farm.token")
	if err := os.WriteFile(defaultPath, []byte(localToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := loadToken("", dir); err != nil || token != localToken {
		t.Fatalf("expected token from data directory, error: %v", err)
	}
	t.Setenv("KEKWFARM_RUNNER_TOKEN", envToken)
	if token, err := loadToken("", dir); err != nil || token != envToken {
		t.Fatalf("expected token from environment, error: %v", err)
	}
	if token, err := loadToken(defaultPath, dir); err != nil || token != localToken {
		t.Fatalf("expected explicit token file to take precedence, error: %v", err)
	}
	if token, err := loadToken(filepath.Join(dir, "missing"), dir); err == nil || token != "" {
		t.Fatal("expected explicit missing token file to fail closed")
	}
	t.Setenv("KEKWFARM_RUNNER_TOKEN", "")
	if err := os.WriteFile(defaultPath, []byte("invalid token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := loadToken("", dir); err == nil || token != "" {
		t.Fatal("expected invalid optional token file to fail closed")
	}
}
