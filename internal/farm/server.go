package farm

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"duckfarm/internal/checker"
	"duckfarm/internal/panel"
	"duckfarm/internal/store"
)

type server struct {
	config         Config
	store          *store.Store
	pattern        *regexp.Regexp
	location       *time.Location
	teams          []team
	wake           chan struct{}
	checkerChanged chan struct{}
	checkerMu      sync.RWMutex
	checker        checkerState
}

type team struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func Run(ctx context.Context, config Config, ready func(net.Addr)) error {
	if err := config.validate(); err != nil {
		return err
	}
	s := &server{
		config: config, wake: make(chan struct{}, 1), checkerChanged: make(chan struct{}, 1),
		pattern: regexp.MustCompile(config.FlagFormat),
		teams:   make([]team, 0, len(config.Teams)),
	}
	s.location, _ = time.LoadLocation(config.Timezone)
	for name, address := range config.Teams {
		s.teams = append(s.teams, team{name, address})
	}
	sort.Slice(s.teams, func(i, j int) bool { return s.teams[i].Name < s.teams[j].Name })

	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	s.store, err = store.Open(ctx, config.Database)
	if err != nil {
		return err
	}
	defer s.store.Close()
	if err := s.loadChecker(ctx); err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second,
	}
	workerContext, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.work(workerContext)
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	log.Printf("KEKWFarm listening on %s", listener.Addr())
	if ready != nil {
		ready(listener.Addr())
	}

	select {
	case err = <-serveDone:
	case <-ctx.Done():
	}
	stopWorker()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := httpServer.Shutdown(shutdown); shutdownErr != nil {
		httpServer.Close()
		if err == nil {
			err = shutdownErr
		}
	}
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /api/login", s.login)
	api.HandleFunc("GET /api/get_config", s.authorizeRunnerOrPassword(s.getConfig))
	api.HandleFunc("GET /api/checker_config", s.authorize(s.getCheckerConfig))
	api.HandleFunc("PUT /api/checker_config", s.authorize(s.putCheckerConfig))
	api.HandleFunc("POST /api/post_flags", s.authorizeRunnerOrPassword(s.postFlags))
	api.HandleFunc("POST /api/runner/heartbeat", s.runnerHeartbeat)
	api.HandleFunc("GET /api/runners", s.authorize(s.listRunners))
	api.HandleFunc("POST /api/runners", s.authorize(s.createRunner))
	api.HandleFunc("DELETE /api/runners/{id}", s.authorize(s.revokeRunner))
	api.HandleFunc("GET /api/filter_flags", s.authorize(s.listFlags))
	api.HandleFunc("GET /api/filter_config", s.authorize(s.filterConfig))
	api.HandleFunc("GET /api/teams", s.authorize(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.teams)
	}))
	api.HandleFunc("GET /api/summary", s.authorize(s.summary))

	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", panel.Handler())
	return mux
}

func (s *server) work(ctx context.Context) {
	period := time.Duration(s.config.SubmitPeriod) * time.Second
	var nextSubmit time.Time
	for ctx.Err() == nil {
		now := time.Now()
		var retain int64
		if s.config.Retention > 0 {
			retain = now.Add(-time.Duration(s.config.Retention) * time.Hour).Unix()
		}
		err := s.store.Maintain(ctx, now.Unix(), now.Unix()-int64(s.config.FlagLifetime), retain)
		active := s.currentChecker()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("maintain flags: %v", err)
			}
		} else if active.submit != nil && !now.Before(nextSubmit) {
			if started := s.submitBatch(ctx, active, time.Now().Unix()); !started.IsZero() {
				nextSubmit = started.Add(period)
				if !time.Now().Before(nextSubmit) {
					continue
				}
			}
		}

		wait := period
		var wake <-chan struct{} = s.wake
		if delay := time.Until(nextSubmit); delay > 0 {
			wait = delay
			wake = nil
		}
		if active.submit == nil {
			wait = time.Minute
			wake = nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-s.checkerChanged:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (s *server) submitBatch(ctx context.Context, active checkerState, now int64) time.Time {
	lease := now + int64(s.config.SubmitTimeout) + 15
	items, err := s.store.Claim(ctx, now, lease, s.config.SubmitLimit)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("claim flags: %v", err)
		}
		return time.Time{}
	}
	if len(items) == 0 {
		return time.Time{}
	}

	flags := make([]string, len(items))
	for i, item := range items {
		flags[i] = item.Flag
	}
	started := time.Now()
	request, cancel := context.WithTimeout(ctx, time.Duration(s.config.SubmitTimeout)*time.Second)
	results, err := active.submit(request, flags)
	cancel()
	if err != nil && ctx.Err() == nil {
		log.Printf("submit batch: %v", err)
	}
	known := make(map[string]checker.Result, len(results))
	for _, result := range results {
		known[result.Flag] = result
	}
	updates := make([]store.Item, 0, len(flags))
	for _, item := range items {
		result, received := known[item.Flag]
		if received && active.config.Protocol == "faust" && result.Status == checker.Queued &&
			time.Since(time.Unix(item.Time, 0)) > 10*time.Second {
			result.Status = checker.Rejected
		}
		response := result.Response
		if !received && err != nil {
			response = "Checker request failed; retry scheduled"
		}
		if len(response) > 2048 {
			response = response[:2048]
		}
		updates = append(updates, store.Item{Flag: item.Flag, Status: result.Status, Response: response})
	}
	save, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := s.store.Resolve(save, updates, lease, time.Now().Unix()+int64(s.config.RetryDelay)); err != nil {
		log.Printf("save submission results: %v", err)
	}
	return started
}
