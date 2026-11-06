package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Defaults struct {
	URL      string
	Password string
	Token    string
	Address  string
}

type settings struct {
	api
	exploit     string
	interpreter string
	pool        int
	period      time.Duration
	shard       int
	shards      int
	single      bool
	endless     bool
	verbose     int
	name        string
	execute     func(context.Context, string, io.Writer) error
}

type target struct {
	name, host string
}

func Run(ctx context.Context, args []string, defaults Defaults) error {
	if defaults.Token == "" {
		defaults.Token = strings.TrimSpace(os.Getenv("KEKWFARM_RUNNER_TOKEN"))
	}
	cfg, err := parse(args, defaults)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	var running atomic.Bool
	if cfg.token != "" {
		heartbeatContext, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			Heartbeat(heartbeatContext, Defaults{URL: cfg.base, Token: cfg.token}, "cli-dev", func() HeartbeatJobs {
				if running.Load() {
					return HeartbeatJobs{Running: 1}
				}
				return HeartbeatJobs{Scheduled: 1}
			})
		}()
		defer func() {
			stop()
			<-done
		}()
	}

	return cfg.withDelivery(func(inbox chan<- foundFlag) error {
		return cfg.loop(ctx, inbox, &running)
	})
}

func (s settings) withDelivery(run func(chan<- foundFlag) error) error {
	inbox := make(chan foundFlag, 512)
	delivery, cancelDelivery := context.WithCancel(context.Background())
	defer cancelDelivery()
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		s.deliver(delivery, inbox)
	}()

	err := run(inbox)
	close(inbox)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-delivered:
	case <-timer.C:
		cancelDelivery()
		<-delivered
		return errors.Join(err, errors.New("flag delivery did not finish"))
	}
	return err
}

func parse(args []string, defaults Defaults) (settings, error) {
	if defaults.URL == "" {
		defaults.URL = "http://127.0.0.1:5137"
	}
	if defaults.Password == "" {
		defaults.Password = os.Getenv("SERVER_PASSWORD")
	}
	cfg := settings{api: api{logger: log.Default(), client: &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}}
	flags := flag.NewFlagSet("duckrunner", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: duckrunner [options] EXPLOIT")
		flags.PrintDefaults()
	}
	flags.StringVar(&cfg.base, "server-url", defaults.URL, "farm URL")
	flags.Var(passwordValue{&cfg.password}, "server-pass", "farm password (defaults to SERVER_PASSWORD)")
	cfg.password = defaults.Password
	tokenFile := flags.String("token-file", "", "file containing a runner token")
	cfg.token = defaults.Token
	flags.StringVar(&cfg.interpreter, "interpreter", "", "explicit executable or interpreter")
	flags.IntVar(&cfg.pool, "pool-size", 50, "maximum simultaneous processes")
	period := flags.Float64("attack-period", 55, "seconds between rounds")
	distribute := flags.String("distribute", "", "run only shard K/N of target hosts")
	flags.BoolVar(&cfg.single, "not-per-team", false, "run once per round with * as the target")
	flags.BoolVar(&cfg.endless, "endless", false, "run one round without per-process deadlines")
	flags.IntVar(&cfg.verbose, "verbose-attacks", 0, "rounds for which to print child output")
	flags.IntVar(&cfg.verbose, "v", 0, "alias for --verbose-attacks")
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 1 {
		return cfg, errors.New("expected one exploit path (see --help)")
	}
	if *tokenFile != "" {
		token, err := ReadTokenFile(*tokenFile)
		if err != nil {
			return cfg, err
		}
		cfg.token = token
	}
	if cfg.pool < 1 || cfg.pool > 10000 || cfg.verbose < 0 ||
		math.IsNaN(*period) || *period < 0.01 || *period > 86400 {
		return cfg, errors.New("invalid pool size, attack period or verbosity")
	}
	cfg.period = time.Duration(*period * float64(time.Second))
	if *distribute != "" {
		var extra string
		n, _ := fmt.Sscanf(*distribute, "%d/%d%s", &cfg.shard, &cfg.shards, &extra)
		if n != 2 || cfg.shards < 2 || cfg.shard < 1 || cfg.shard > cfg.shards ||
			*distribute != fmt.Sprintf("%d/%d", cfg.shard, cfg.shards) || cfg.single {
			return cfg, errors.New("use --distribute K/N (1 <= K <= N, N >= 2) separately from --not-per-team")
		}
	}
	if !strings.Contains(cfg.base, "://") {
		cfg.base = "http://" + cfg.base
	}
	u, err := url.Parse(cfg.base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, errors.New("invalid server URL")
	}
	cfg.base = strings.TrimRight(cfg.base, "/")
	if cfg.token != "" {
		if !ValidToken(cfg.token) {
			return cfg, errors.New("invalid runner token")
		}
	} else if strings.TrimSpace(cfg.password) == "" {
		return cfg, errors.New("set a runner token or SERVER_PASSWORD")
	}

	cfg.exploit, err = filepath.Abs(flags.Arg(0))
	if err != nil {
		return cfg, err
	}
	file, err := os.Stat(cfg.exploit)
	if err != nil {
		return cfg, err
	}
	if !file.Mode().IsRegular() {
		return cfg, errors.New("exploit path must be a regular file")
	}
	return cfg, nil
}

func ReadTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", errors.New("runner token file must be a small regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(data) > 4096 || !ValidToken(token) {
		return "", errors.New("invalid runner token file")
	}
	return token, nil
}

func ValidToken(token string) bool {
	if len(token) != 67 || !strings.HasPrefix(token, "kr_") {
		return false
	}
	_, err := hex.DecodeString(token[3:])
	return err == nil
}

// The flag package prints default values in usage, so keep the password out of String.
type passwordValue struct{ dst *string }

func (p passwordValue) String() string { return "" }
func (p passwordValue) Set(value string) error {
	*p.dst = value
	return nil
}

func (s settings) loop(ctx context.Context, inbox chan<- foundFlag, running *atomic.Bool) error {
	var config remoteConfig
	var pattern *regexp.Regexp
	for round := 1; ctx.Err() == nil; round++ {
		fresh, err := s.getConfig(ctx)
		if err == nil {
			var compiled *regexp.Regexp
			compiled, err = regexp.Compile(fresh.FlagFormat)
			if err == nil && fresh.FlagFormat == "" {
				err = errors.New("server returned an empty flag format")
			}
			if err == nil {
				config, pattern = fresh, compiled
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if pattern == nil {
				return fmt.Errorf("get farm configuration: %w", err)
			}
			s.logger.Printf("Using previous farm configuration: %v", err)
		}

		targets := s.targets(config.Teams)
		if len(targets) == 0 {
			s.logger.Print("No targets in this round")
		}
		deadline := time.Now().Add(s.period)
		running.Store(len(targets) > 0)
		s.round(ctx, deadline, targets, pattern, inbox, round <= s.verbose)
		running.Store(false)
		if s.endless {
			return nil
		}
		if !pause(ctx, time.Until(deadline)) {
			break
		}
	}
	return nil
}

func (s settings) targets(teams map[string]string) []target {
	if s.single {
		return []target{{"*", "*"}}
	}
	targets := make([]target, 0, len(teams))
	for name, host := range teams {
		if s.shards == 0 || uint64(crc32.ChecksumIEEE([]byte(host)))%uint64(s.shards) == uint64(s.shard-1) {
			targets = append(targets, target{name, host})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets
}

func (s settings) round(ctx context.Context, deadline time.Time, targets []target, pattern *regexp.Regexp, inbox chan<- foundFlag, verbose bool) {
	if len(targets) == 0 {
		return
	}
	workers := min(s.pool, len(targets))
	waves := (len(targets) + workers - 1) / workers
	budget := s.period / time.Duration(waves)
	if !s.endless {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	_ = s.runTargets(ctx, targets, budget, pattern, inbox, verbose)
}

func (s settings) runTargets(ctx context.Context, targets []target, budget time.Duration, pattern *regexp.Regexp, inbox chan<- foundFlag, verbose bool) error {
	var running sync.WaitGroup
	var failed atomic.Int64
	jobs := make(chan target)
	for range min(s.pool, len(targets)) {
		running.Go(func() {
			for target := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := s.runTarget(ctx, target, budget, pattern, inbox, verbose); err != nil {
					failed.Add(1)
					if ctx.Err() == nil {
						s.logger.Printf("%s: %v", target.name, err)
					}
				}
			}
		})
	}
send:
	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	running.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if count := failed.Load(); count != 0 {
		return fmt.Errorf("%d of %d target runs failed", count, len(targets))
	}
	return nil
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
