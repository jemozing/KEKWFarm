package runnerweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"duckfarm/internal/runner"
)

const (
	connectionFilename = "connection.json"
	maxConnectionURL   = 4096
	maxConnectionFile  = 8192
)

var (
	errInvalidFarmURL        = errors.New("enter a valid HTTP or HTTPS farm URL without credentials, query or fragment")
	errInvalidFarmToken      = errors.New("invalid runner token")
	errInvalidConnectionFile = errors.New("invalid runner connection settings file")
)

type connectionConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// HasSavedConnection reports whether the WebUI has saved its own connection settings.
func HasSavedConnection(directory string) (bool, error) {
	_, err := os.Lstat(filepath.Join(directory, connectionFilename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func validateConnection(config connectionConfig) (connectionConfig, error) {
	address := config.URL
	if address == "" || len(address) > maxConnectionURL || strings.ContainsAny(address, "\\#") ||
		strings.IndexFunc(address, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return connectionConfig{}, errInvalidFarmURL
	}
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || strings.HasSuffix(parsed.Host, ":") {
		return connectionConfig{}, errInvalidFarmURL
	}
	if strings.HasPrefix(parsed.Host, "[") {
		ip, err := netip.ParseAddr(parsed.Hostname())
		if err != nil || !ip.Is6() {
			return connectionConfig{}, errInvalidFarmURL
		}
	} else if strings.Count(parsed.Host, ":") > 1 {
		return connectionConfig{}, errInvalidFarmURL
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return connectionConfig{}, errInvalidFarmURL
		}
	}
	config.URL = strings.TrimRight(parsed.String(), "/")
	if len(config.URL) > maxConnectionURL {
		return connectionConfig{}, errInvalidFarmURL
	}
	if config.Token != "" && !runner.ValidToken(config.Token) {
		return connectionConfig{}, errInvalidFarmToken
	}
	return config, nil
}

func loadConnection(directory string, defaults connectionConfig) (connectionConfig, error) {
	path := filepath.Join(directory, connectionFilename)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if defaults.URL == "" {
			defaults.URL = "http://127.0.0.1:5137"
		}
		return validateConnection(defaults)
	}
	if err != nil {
		return connectionConfig{}, fmt.Errorf("inspect runner connection settings: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConnectionFile {
		return connectionConfig{}, errInvalidConnectionFile
	}
	file, err := os.Open(path)
	if err != nil {
		return connectionConfig{}, fmt.Errorf("open runner connection settings: %w", err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return connectionConfig{}, fmt.Errorf("inspect runner connection settings: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConnectionFile {
		return connectionConfig{}, errInvalidConnectionFile
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxConnectionFile+1))
	decoder.DisallowUnknownFields()
	var config connectionConfig
	if err := decoder.Decode(&config); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return connectionConfig{}, errInvalidConnectionFile
	}
	if config.Token != "" && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return connectionConfig{}, errors.New("runner connection settings with a token must be private")
	}
	return validateConnection(config)
}

func saveConnection(directory string, config connectionConfig) error {
	config, err := validateConnection(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runner data directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".connection-")
	if err != nil {
		return fmt.Errorf("create runner connection settings: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("set runner connection settings permissions: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("write runner connection settings: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync runner connection settings: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runner connection settings: %w", err)
	}
	if err := os.Rename(file.Name(), filepath.Join(directory, connectionFilename)); err != nil {
		return fmt.Errorf("replace runner connection settings: %w", err)
	}
	return nil
}
