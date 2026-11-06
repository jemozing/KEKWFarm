package farm

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"duckfarm/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("write API response: %v", err)
	}
}

func problem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("API: %v", err)
	problem(w, http.StatusInternalServerError, "Database error")
}

func (s *server) accepts(password string) bool {
	provided := sha256.Sum256([]byte(password))
	expected := sha256.Sum256([]byte(s.config.Password))
	return subtle.ConstantTimeCompare(provided[:], expected[:]) == 1
}

func (s *server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.accepts(r.Header.Get("Authorization")) {
			problem(w, http.StatusForbidden, "Invalid password")
			return
		}
		next(w, r)
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct{ Password string }
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input)
	if err != nil && err != io.EOF {
		problem(w, http.StatusBadRequest, "Invalid login request")
		return
	}
	if !s.accepts(input.Password) && !s.accepts(r.Header.Get("Authorization")) {
		problem(w, http.StatusForbidden, "Invalid password")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"TEAMS": s.config.Teams, "FLAG_FORMAT": s.config.FlagFormat,
		"FLAG_LIFETIME": s.config.FlagLifetime, "SUBMIT_PERIOD": s.config.SubmitPeriod,
		"SUBMIT_FLAG_LIMIT": s.config.SubmitLimit, "TIMEZONE": s.config.Timezone,
	})
}

func (s *server) postFlags(w http.ResponseWriter, r *http.Request) {
	const maxFlags = 10000
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem(w, http.StatusRequestEntityTooLarge, "Flag batch is too large")
		} else {
			problem(w, http.StatusBadRequest, "Could not read flag batch")
		}
		return
	}
	raw = bytes.TrimSpace(raw)
	var flags []store.Item
	manual := len(raw) > 0 && raw[0] == '{'
	if manual {
		var input struct{ Text string }
		if err := json.Unmarshal(raw, &input); err != nil {
			problem(w, http.StatusBadRequest, "Invalid flag text")
			return
		}
		for _, line := range strings.Split(input.Text, "\n") {
			for _, flag := range s.pattern.FindAllString(strings.TrimSuffix(line, "\r"), maxFlags+1-len(flags)) {
				flags = append(flags, store.Item{Flag: flag, Sploit: "Manual", Team: "*"})
			}
			if len(flags) > maxFlags {
				break
			}
		}
	} else if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &flags) != nil {
		problem(w, http.StatusBadRequest, "Expected an array of flags")
		return
	}
	if len(flags) > maxFlags {
		problem(w, http.StatusRequestEntityTooLarge, "Flag batch is too large")
		return
	}
	accepted := flags[:0]
	validate := s.currentChecker().validate
	for _, item := range flags {
		if item.Flag == "" || len(item.Flag) > 8192 || len(item.Sploit) > 255 || len(item.Team) > 255 {
			problem(w, http.StatusBadRequest, "Invalid flag, exploit or team")
			return
		}
		if validate != nil {
			item.Flag, err = validate(item.Flag)
			if err != nil {
				continue
			}
		}
		accepted = append(accepted, item)
	}
	if manual && len(accepted) == 0 {
		problem(w, http.StatusBadRequest, "No matching flags found")
		return
	}
	if err := s.store.Add(r.Context(), accepted, time.Now().Unix()); err != nil {
		internalError(w, err)
		return
	}
	if len(accepted) != 0 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": len(accepted)})
}

func (s *server) filterConfig(w http.ResponseWriter, r *http.Request) {
	filters, err := s.store.Distinct(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filters": filters, "flag_format": s.config.FlagFormat, "server_tz": s.config.Timezone,
	})
}

func (s *server) filter(query url.Values) (store.Filter, error) {
	f := store.Filter{
		Sploit: query.Get("sploit"), Team: query.Get("team"), Status: query.Get("status"),
		Flag: query.Get("flag"), Response: query.Get("checksystem_response"),
	}
	for name, destination := range map[string]**int64{"since": &f.Since, "until": &f.Until} {
		if value := strings.TrimSpace(query.Get(name)); value != "" {
			date, err := time.ParseInLocation("2006-01-02 15:04", value, s.location)
			if err != nil {
				return f, errors.New("Dates must use YYYY-MM-DD HH:MM")
			}
			unix := date.Unix()
			*destination = &unix
		}
	}
	if value := query.Get("last"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 || seconds > 315360000 {
			return f, errors.New("Invalid last interval")
		}
		unix := time.Now().Unix() - seconds
		if f.Since == nil || unix > *f.Since {
			f.Since = &unix
		}
	}
	return f, nil
}

func (s *server) listFlags(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, err := strconv.Atoi(query.Get("page"))
	if query.Get("page") == "" {
		page, err = 1, nil
	}
	if err != nil || page < 1 || page > 100000000 {
		problem(w, http.StatusBadRequest, "Invalid page")
		return
	}
	size, err := strconv.Atoi(query.Get("page_size"))
	if query.Get("page_size") == "" {
		size, err = 30, nil
	}
	if err != nil || size < 1 || size > 100 || page-1 > int(^uint(0)>>1)/size {
		problem(w, http.StatusBadRequest, "Invalid page size or offset")
		return
	}
	filter, err := s.filter(query)
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	flags, total, err := s.store.List(r.Context(), filter, size, (page-1)*size)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags, "page": page, "page_size": size, "total": total})
}

func (s *server) summary(w http.ResponseWriter, r *http.Request) {
	filter, err := s.filter(r.URL.Query())
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	counts, err := s.store.Summary(r.Context(), filter, r.URL.Query().Get("response") != "")
	if err != nil {
		internalError(w, err)
		return
	}
	type teamStatus struct {
		Team   string `json:"team"`
		Status string `json:"team_status"`
	}
	type exploitStatus struct {
		Sploit string       `json:"sploit_name"`
		Teams  []teamStatus `json:"teams"`
	}
	grouped := make(map[string]map[string]string)
	for _, count := range counts {
		if grouped[count.Sploit] == nil {
			grouped[count.Sploit] = make(map[string]string)
		}
		grouped[count.Sploit][count.Team] += count.Key + ": " + strconv.Itoa(count.Total) + "\n"
	}
	result := make([]exploitStatus, 0, len(grouped))
	for name, statuses := range grouped {
		for _, team := range s.teams {
			if _, exists := statuses[team.Name]; !exists {
				statuses[team.Name] = "not status"
			}
		}
		entry := exploitStatus{Sploit: name, Teams: make([]teamStatus, 0, len(statuses))}
		for name, status := range statuses {
			entry.Teams = append(entry.Teams, teamStatus{name, status})
		}
		sort.Slice(entry.Teams, func(i, j int) bool { return entry.Teams[i].Team < entry.Teams[j].Team })
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sploit < result[j].Sploit })
	writeJSON(w, http.StatusOK, result)
}
