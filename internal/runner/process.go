package runner

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxLine = 256 << 10

func (s settings) runTarget(ctx context.Context, target target, budget time.Duration, pattern *regexp.Regexp, inbox chan<- foundFlag, verbose bool) error {
	processContext := ctx
	if !s.endless {
		var cancel context.CancelFunc
		processContext, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	name := s.name
	if name == "" {
		name = filepath.Base(s.exploit)
	}
	out := &output{pattern: pattern, logger: s.logger, emit: func(flag string) {
		if len(flag) > 8192 {
			return
		}
		item := foundFlag{Flag: strings.Clone(flag), Sploit: name, Team: target.name}
		select {
		case inbox <- item:
		default:
			select {
			case inbox <- item:
			case <-processContext.Done():
			}
		}
	}}
	if verbose {
		out.verbose = 10
		if s.execute != nil {
			out.verbose = 100
		}
		out.team = target.name
	}
	execute := s.execute
	if execute == nil {
		execute = s.executeLocal
	}
	err := execute(processContext, target.host, out)
	out.finish()
	if err != nil && processContext.Err() != nil {
		return processContext.Err()
	}
	return err
}

func (s settings) executeLocal(ctx context.Context, host string, output io.Writer) error {
	args := []string{s.exploit, host}
	if s.interpreter != "" {
		args = append([]string{s.interpreter}, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PYTHONUNBUFFERED=") &&
			!strings.HasPrefix(entry, "SERVER_PASSWORD=") &&
			!strings.HasPrefix(entry, "KEKWFARM_RUNNER_TOKEN=") &&
			!strings.HasPrefix(entry, "KEKWFARM_RUNNER_TOKEN_FILE=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")
	cmd.Stdout, cmd.Stderr = output, output
	return cmd.Run()
}

type output struct {
	line    []byte
	dropped bool
	pattern *regexp.Regexp
	emit    func(string)
	verbose int
	team    string
	logger  *log.Logger
}

func (o *output) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		line, rest, newline := bytes.Cut(data, []byte{'\n'})
		if len(line) > maxLine-len(o.line) {
			o.dropped = true
		}
		if !o.dropped {
			o.line = append(o.line, line...)
		}
		if newline {
			o.finish()
		}
		data = rest
	}
	return n, nil
}

func (o *output) finish() {
	if !o.dropped && len(o.line) > 0 {
		line := strings.TrimSuffix(string(o.line), "\r")
		for _, flag := range o.pattern.FindAllString(line, -1) {
			if flag != "" {
				o.emit(flag)
			}
		}
		if o.verbose > 0 {
			o.verbose--
			o.logger.Printf("%s: %.500s", o.team, line)
		}
	}
	o.line = o.line[:0]
	o.dropped = false
}
