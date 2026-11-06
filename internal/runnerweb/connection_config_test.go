package runnerweb

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateConnectionCanonicalURL(t *testing.T) {
	token := "kr_" + strings.Repeat("a", 64)
	for _, test := range []struct{ input, want string }{
		{"127.0.0.1:5137", "http://127.0.0.1:5137"},
		{"farm:5137/reverse/proxy///", "http://farm:5137/reverse/proxy"},
		{"https://farm.example/farm%20ui/", "https://farm.example/farm%20ui"},
		{"https://farm.example/prefix%2Fv1/", "https://farm.example/prefix%2Fv1"},
		{"https://[::1]:5137/gateway/", "https://[::1]:5137/gateway"},
	} {
		got, err := validateConnection(connectionConfig{URL: test.input, Token: token})
		if err != nil || got.URL != test.want || got.Token != token {
			t.Fatalf("validate farm URL %q = %q, %v", test.input, got.URL, err)
		}
	}
}

func TestValidateConnectionRejectsBadInputWithoutEchoingSecrets(t *testing.T) {
	const secret = "sensitive-value"
	for _, address := range []string{
		"", "http:///path", "ftp://farm.example", "https://farm.example:0", "https://farm.example:65536", "https://farm.example:",
		"https://farm.example/query?", "https://farm.example/query?token=" + secret, "https://farm.example/path#",
		"https://user:" + secret + "@farm.example", "https://" + secret + "@farm.example/%zz",
		" https://farm.example", "https://farm.example/path with space", "https://farm.example/path\u00a0value",
		"https://farm.example\\path", "https://2001:db8::1", "https://[farm.example]",
	} {
		got, err := validateConnection(connectionConfig{URL: address})
		if !errors.Is(err, errInvalidFarmURL) || got != (connectionConfig{}) || strings.Contains(err.Error(), secret) {
			t.Fatalf("expected redacted invalid URL error, got %v", err)
		}
	}
	got, err := validateConnection(connectionConfig{URL: "https://farm.example", Token: secret})
	if !errors.Is(err, errInvalidFarmToken) || got != (connectionConfig{}) || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected redacted invalid token error, got %v", err)
	}
}

func TestConnectionFileOverridesStartupDefaultsAndStaysPrivate(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "runner-data")
	defaults := connectionConfig{URL: "initial.example:5137/", Token: "kr_" + strings.Repeat("a", 64)}
	if got, err := loadConnection(directory, defaults); err != nil || got.URL != "http://initial.example:5137" || got.Token != defaults.Token {
		t.Fatalf("load startup defaults: %q, %v", got.URL, err)
	}
	configured := connectionConfig{URL: "https://other.example/proxy/", Token: "kr_" + strings.Repeat("b", 64)}
	if err := saveConnection(directory, configured); err != nil {
		t.Fatal(err)
	}
	configured.URL = strings.TrimSuffix(configured.URL, "/")
	if got, err := loadConnection(directory, defaults); err != nil || got != configured {
		t.Fatalf("reload saved settings: %q, %v", got.URL, err)
	}
	if err := saveConnection(directory, connectionConfig{URL: "https://farm.example?invalid"}); !errors.Is(err, errInvalidFarmURL) {
		t.Fatalf("invalid settings must not overwrite the saved pair: %v", err)
	}
	if got, err := loadConnection(directory, defaults); err != nil || got != configured {
		t.Fatalf("previous settings not retained: %q, %v", got.URL, err)
	}
	configured.Token = ""
	if err := saveConnection(directory, configured); err != nil {
		t.Fatal(err)
	}
	if got, err := loadConnection(directory, defaults); err != nil || got != configured {
		t.Fatalf("cleared token fell back to startup token: %q, %v", got.URL, err)
	}
	info, err := os.Stat(filepath.Join(directory, connectionFilename))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected connection settings permissions: %o", info.Mode().Perm())
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || files[0].Name() != connectionFilename {
		t.Fatalf("save leaked temporary files: %v, %v", files, err)
	}
}

func TestLoadConnectionRejectsMalformedFileWithoutEchoingSecrets(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, connectionFilename)
	const secret = "sensitive-value"
	for _, data := range []string{
		`{"url":"https://farm.example","` + secret + `":"value"}`,
		`{"url":"https://farm.example"} {"token":"` + secret + `"}`,
		strings.Repeat(" ", maxConnectionFile+1),
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := loadConnection(directory, connectionConfig{}); !errors.Is(err, errInvalidConnectionFile) || got != (connectionConfig{}) || strings.Contains(err.Error(), secret) {
			t.Fatalf("expected redacted settings error, got %v", err)
		}
	}
}
