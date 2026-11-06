package farm

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"

	"duckfarm/internal/store"
)

const runnerOnlineWindow = 30 * time.Second

func (s *server) authorizeRunnerOrPassword(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.accepts(r.Header.Get("Authorization")) {
			next(w, r)
			return
		}
		if _, ok := s.authenticateRunner(w, r); ok {
			next(w, r)
		}
	}
}

func (s *server) authenticateRunner(w http.ResponseWriter, r *http.Request) (string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	scheme, value, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	token := strings.TrimLeft(value, " ")
	if !strings.EqualFold(scheme, "Bearer") || len(token) != 67 || !strings.HasPrefix(token, "kr_") {
		problem(w, http.StatusForbidden, "Invalid runner credentials")
		return "", false
	}
	id, err := s.store.RunnerID(r.Context(), token)
	if errors.Is(err, store.ErrRunnerNotFound) {
		problem(w, http.StatusForbidden, "Invalid runner credentials")
		return "", false
	}
	if err != nil {
		internalError(w, err)
		return "", false
	}
	return id, true
}

func (s *server) listRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.store.ListRunners(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	if runners == nil {
		runners = []store.Runner{}
	}
	cutoff := time.Now().Add(-runnerOnlineWindow)
	for i := range runners {
		item := &runners[i]
		item.Online = !item.Revoked && item.LastSeen != nil && item.LastSeen.After(cutoff)
	}
	writeJSON(w, http.StatusOK, runners)
}

func (s *server) createRunner(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !readRunnerJSON(w, r, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if !validRunnerText(name, 100) {
		problem(w, http.StatusBadRequest, "Enter a runner name up to 100 bytes")
		return
	}
	item, token, err := s.store.CreateRunner(r.Context(), name)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Runner store.Runner `json:"runner"`
		Token  string       `json:"token"`
	}{Runner: item, Token: token})
}

func (s *server) revokeRunner(w http.ResponseWriter, r *http.Request) {
	err := s.store.RevokeRunner(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrRunnerNotFound) {
		problem(w, http.StatusNotFound, "Runner not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) runnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authenticateRunner(w, r)
	if !ok {
		return
	}
	var input struct {
		Version string           `json:"version"`
		Address string           `json:"address"`
		Jobs    store.RunnerJobs `json:"jobs"`
	}
	if !readRunnerJSON(w, r, &input) {
		return
	}
	if !validRunnerText(input.Version, 64) || !validRunnerJobs(input.Jobs) {
		problem(w, http.StatusBadRequest, "Invalid runner version or job counts")
		return
	}
	address := runnerPeerAddress(r.RemoteAddr)
	if input.Address != "" {
		ip, err := netip.ParseAddr(input.Address)
		if err != nil || ip.Zone() != "" || ip.Unmap().IsUnspecified() || ip.Unmap().IsMulticast() {
			problem(w, http.StatusBadRequest, "Invalid runner IP address")
			return
		}
		address = ip.Unmap().String()
	}
	err := s.store.UpdateRunner(r.Context(), id, input.Version, address, input.Jobs)
	if errors.Is(err, store.ErrRunnerNotFound) {
		problem(w, http.StatusForbidden, "Invalid runner credentials")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func runnerPeerAddress(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	ip, err := netip.ParseAddr(remote)
	if err != nil {
		return ""
	}
	return ip.WithZone("").Unmap().String()
}

func readRunnerJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err == nil && decoder.Decode(new(json.RawMessage)) == io.EOF {
		return true
	}
	problem(w, http.StatusBadRequest, "Invalid runner JSON")
	return false
}

func validRunnerText(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.IndexFunc(value, unicode.IsControl) == -1
}

func validRunnerJobs(jobs store.RunnerJobs) bool {
	total := 0
	for _, count := range []int{jobs.Scheduled, jobs.Running, jobs.Stopping, jobs.Completed, jobs.Failed, jobs.Stopped} {
		if count < 0 || count > 10000 {
			return false
		}
		total += count
	}
	return total <= 10000
}
