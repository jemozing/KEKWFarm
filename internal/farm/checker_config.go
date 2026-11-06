package farm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"duckfarm/internal/checker"
)

const maxCheckerConfigSize = 64 << 10

type checkerState struct {
	config   checker.Config
	submit   checker.Submit
	validate func(string) (string, error)
}

func compileChecker(config checker.Config) (checkerState, error) {
	state := checkerState{config: config}
	var err error
	if config.Protocol != "" {
		state.submit, err = checker.New(config)
		if err != nil {
			return checkerState{}, err
		}
	}
	state.validate, err = checker.IngestValidator(config)
	return state, err
}

func (s *server) loadChecker(ctx context.Context) error {
	data, err := s.store.LoadCheckerConfig(ctx)
	if err != nil {
		return fmt.Errorf("load saved checker configuration: %w", err)
	}
	config := s.config.Checker
	if data != nil {
		config = checker.Config{}
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("decode saved checker configuration: %w", err)
		}
	}
	s.checker, err = compileChecker(config)
	return err
}

func (s *server) currentChecker() checkerState {
	s.checkerMu.RLock()
	defer s.checkerMu.RUnlock()
	return s.checker
}

func (s *server) getCheckerConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.currentChecker().config)
}

func (s *server) putCheckerConfig(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCheckerConfigSize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, "Checker configuration is too large")
		} else {
			problem(w, http.StatusBadRequest, "Could not read checker configuration")
		}
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		problem(w, http.StatusBadRequest, "Expected a single checker configuration object")
		return
	}
	var config checker.Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		problem(w, http.StatusBadRequest, "Invalid checker configuration: "+err.Error())
		return
	}
	state, err := compileChecker(config)
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		internalError(w, err)
		return
	}

	// Serialize saves so the active configuration always matches the latest commit.
	s.checkerMu.Lock()
	err = s.store.SaveCheckerConfig(r.Context(), canonical)
	if err == nil {
		s.checker = state
	}
	s.checkerMu.Unlock()
	if err != nil {
		log.Printf("save checker configuration: %v", err)
		problem(w, http.StatusInternalServerError, "Could not save checker configuration")
		return
	}
	s.notifySettingsChanged()
	writeJSON(w, http.StatusOK, config)
}
