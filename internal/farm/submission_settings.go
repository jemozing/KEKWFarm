package farm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

type submissionSettings struct {
	FlagFormat   string `json:"flag_format"`
	FlagLifetime int    `json:"flag_lifetime_seconds"`
	SubmitPeriod int    `json:"submit_period_seconds"`
	SubmitLimit  int    `json:"submit_flag_limit"`
}

type submissionState struct {
	submissionSettings
	pattern *regexp.Regexp
}

func (c Config) submissionSettings() submissionSettings {
	return submissionSettings{
		FlagFormat: c.FlagFormat, FlagLifetime: c.FlagLifetime,
		SubmitPeriod: c.SubmitPeriod, SubmitLimit: c.SubmitLimit,
	}
}

func (settings submissionSettings) compile() (submissionState, error) {
	if settings.FlagFormat == "" {
		return submissionState{}, errors.New("flag_format must not be empty")
	}
	pattern, err := regexp.Compile(settings.FlagFormat)
	if err != nil {
		return submissionState{}, fmt.Errorf("flag_format: %w", err)
	}
	if settings.FlagLifetime < 1 || settings.FlagLifetime > 604800 {
		return submissionState{}, errors.New("flag_lifetime_seconds must be between 1 and 604800")
	}
	if settings.SubmitPeriod < 1 || settings.SubmitPeriod > 86400 {
		return submissionState{}, errors.New("submit_period_seconds must be between 1 and 86400")
	}
	if settings.SubmitLimit < 1 || settings.SubmitLimit > 10000 {
		return submissionState{}, errors.New("submit_flag_limit must be between 1 and 10000")
	}
	return submissionState{submissionSettings: settings, pattern: pattern}, nil
}

func (s *server) loadSubmissionSettings(ctx context.Context) error {
	data, err := s.store.LoadSubmissionSettings(ctx)
	if err != nil {
		return fmt.Errorf("load saved submission settings: %w", err)
	}
	settings := s.config.submissionSettings()
	if data != nil {
		settings = submissionSettings{}
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("decode saved submission settings: %w", err)
		}
	}
	s.submission, err = settings.compile()
	return err
}

func (s *server) currentSubmission() submissionState {
	s.submissionMu.RLock()
	defer s.submissionMu.RUnlock()
	return s.submission
}

func (s *server) getSubmissionSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.currentSubmission().submissionSettings)
}

func (s *server) putSubmissionSettings(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var settings *submissionSettings
	if err := decoder.Decode(&settings); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, "Submission settings are too large")
		} else {
			problem(w, http.StatusBadRequest, "Invalid submission settings: "+err.Error())
		}
		return
	}
	var extra json.RawMessage
	if settings == nil || decoder.Decode(&extra) != io.EOF {
		problem(w, http.StatusBadRequest, "Expected a single submission settings object")
		return
	}
	state, err := settings.compile()
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := json.Marshal(settings)
	if err != nil {
		internalError(w, err)
		return
	}
	// Keep the database and active snapshot in the same order across concurrent saves.
	s.submissionMu.Lock()
	err = s.store.SaveSubmissionSettings(r.Context(), data)
	if err == nil {
		s.submission = state
	}
	s.submissionMu.Unlock()
	if err != nil {
		internalError(w, err)
		return
	}
	s.notifySettingsChanged()
	writeJSON(w, http.StatusOK, settings)
}
