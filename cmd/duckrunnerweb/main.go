package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"duckfarm/internal/runner"
	"duckfarm/internal/runnerdocker"
	"duckfarm/internal/runnerweb"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:5138", "WebUI listen address")
	data := flag.String("data", "runner-data", "directory for uploaded jobs")
	farmURLDefault := os.Getenv("KEKWFARM_FARM_URL")
	if farmURLDefault == "" {
		farmURLDefault = "http://127.0.0.1:5137"
	}
	farmURL := flag.String("farm-url", farmURLDefault, "farm URL (defaults to KEKWFARM_FARM_URL)")
	config := flag.String("config", "duckfarm.json", "farm config with the shared password")
	tokenFile := flag.String("token-file", os.Getenv("KEKWFARM_RUNNER_TOKEN_FILE"), "runner token file (defaults to KEKWFARM_RUNNER_TOKEN_FILE)")
	dockerSocket := flag.String("docker-socket", "/var/run/docker.sock", "Docker Engine Unix socket")
	workerNetwork := flag.String("worker-network", "bridge", "Docker network for uploaded programs")
	engine := flag.Bool("engine", false, "serve only the private runner engine socket")
	control := flag.String("control", "/control", "runner engine socket and ownership directory")
	brokerSocket := flag.String("broker-socket", "/control/engine.sock", "private runner engine socket")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *engine {
		if err := runnerdocker.ServeBroker(ctx, runnerdocker.BrokerConfig{
			Uploads: *data, Control: *control, DockerSocket: *dockerSocket, Network: *workerNetwork,
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	address, err := runner.NormalizeAdvertisedAddress(os.Getenv("KEKWFARM_RUNNER_ADDRESS"))
	if err != nil {
		log.Fatalf("KEKWFARM_RUNNER_ADDRESS: %v", err)
	}
	password, err := loadPassword(*config)
	if err != nil {
		log.Fatal(err)
	}
	savedConnection, err := runnerweb.HasSavedConnection(*data)
	if err != nil {
		log.Fatal(err)
	}
	var token string
	if !savedConnection {
		token, err = loadToken(*tokenFile, *data)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := runnerweb.Run(ctx, runnerweb.Config{
		Listen: *listen, Directory: *data, FarmURL: *farmURL, Password: password,
		BrokerSocket: *brokerSocket, Token: token, Version: buildVersion(), Address: address,
	}); err != nil {
		log.Fatal(err)
	}
}

func loadToken(path, dataDir string) (string, error) {
	value := os.Getenv("KEKWFARM_RUNNER_TOKEN")
	if path == "" && value == "" {
		path = filepath.Join(dataDir, "farm.token")
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return "", nil
		} else if err != nil {
			return "", fmt.Errorf("inspect runner token file: %w", err)
		}
	}
	if path != "" {
		token, err := runner.ReadTokenFile(path)
		if err != nil {
			return "", fmt.Errorf("read runner token file: %w", err)
		}
		return token, nil
	}
	if !runner.ValidToken(value) {
		return "", errors.New("invalid KEKWFARM_RUNNER_TOKEN")
	}
	return value, nil
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
				return "dev-" + setting.Value[:12]
			}
		}
	}
	return "dev"
}

func loadPassword(path string) (string, error) {
	password := os.Getenv("SERVER_PASSWORD")
	if password == "" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		var cfg struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(f).Decode(&cfg); err != nil {
			return "", fmt.Errorf("read farm configuration: %w", err)
		}
		password = cfg.Password
	}
	if password == "" || strings.TrimSpace(password) != password || strings.ContainsAny(password, "\r\n") {
		return "", fmt.Errorf("set a valid SERVER_PASSWORD or password in config")
	}
	return password, nil
}
