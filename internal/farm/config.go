package farm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"duckfarm/internal/checker"
)

type Config struct {
	Listen        string            `json:"listen"`
	Database      string            `json:"database"`
	Password      string            `json:"password"`
	Teams         map[string]string `json:"teams"`
	FlagFormat    string            `json:"flag_format"`
	FlagLifetime  int               `json:"flag_lifetime_seconds"`
	SubmitPeriod  int               `json:"submit_period_seconds"`
	SubmitLimit   int               `json:"submit_flag_limit"`
	SubmitTimeout int               `json:"submit_timeout_seconds"`
	RetryDelay    int               `json:"retry_delay_seconds"`
	Retention     int               `json:"history_retention_hours"`
	Timezone      string            `json:"timezone"`
	Checker       checker.Config    `json:"checker"`
}

func Load(path string) (Config, error) {
	cfg := Config{
		Listen: "127.0.0.1:5137", Database: "duckfarm.sqlite",
		Teams: map[string]string{}, FlagFormat: "[A-Z0-9]{31}=",
		FlagLifetime: 300, SubmitPeriod: 2, SubmitLimit: 100,
		SubmitTimeout: 30, RetryDelay: 2, Retention: 24, Timezone: "UTC",
	}
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("config must contain a single JSON object")
	}
	if password := os.Getenv("SERVER_PASSWORD"); password != "" {
		cfg.Password = password
	}
	if listen := os.Getenv("KEKWFARM_LISTEN"); listen != "" {
		cfg.Listen = listen
	}
	if database := os.Getenv("KEKWFARM_DATABASE"); database != "" {
		cfg.Database = database
	}
	if cfg.Teams == nil {
		cfg.Teams = map[string]string{}
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Password) == "" || strings.TrimSpace(c.Password) != c.Password ||
		strings.ContainsAny(c.Password, "\r\n") {
		return fmt.Errorf("set a valid SERVER_PASSWORD or password in config")
	}
	if c.Database == "" || c.Listen == "" {
		return fmt.Errorf("database and listen must be set")
	}
	for name, address := range c.Teams {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(address) == "" {
			return fmt.Errorf("team names and addresses must be non-empty")
		}
	}
	if _, err := c.submissionSettings().compile(); err != nil {
		return err
	}
	if c.SubmitTimeout < 1 || c.SubmitTimeout > 3600 ||
		c.RetryDelay < 1 || c.RetryDelay > 86400 ||
		c.Retention < 0 || c.Retention > 87600 {
		return fmt.Errorf("invalid timeout, retry delay or history retention")
	}
	if c.Timezone == "" {
		return fmt.Errorf("timezone must be set")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	return nil
}
