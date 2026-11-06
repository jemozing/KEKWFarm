package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"time"
)

type RoundOptions struct {
	Name        string
	Path        string
	Parallelism int
	Timeout     time.Duration
	Execute     func(context.Context, string, io.Writer) error
}

// RunOnce executes one round against the farm's current teams and forwards its flags.
func RunOnce(ctx context.Context, defaults Defaults, options RoundOptions, output io.Writer) error {
	args := []string{"--pool-size", strconv.Itoa(options.Parallelism)}
	if options.Timeout <= 0 || options.Execute == nil {
		return errors.New("a target executor and positive timeout are required")
	}
	cfg, err := parse(append(args, "--", options.Path), defaults)
	if err != nil {
		return err
	}
	cfg.name = options.Name
	cfg.execute = options.Execute
	cfg.logger = log.New(output, "", log.LstdFlags|log.LUTC)

	config, err := cfg.getConfig(ctx)
	if err != nil {
		return fmt.Errorf("get farm configuration: %w", err)
	}
	if config.FlagFormat == "" {
		return errors.New("farm returned an empty flag format")
	}
	pattern, err := regexp.Compile(config.FlagFormat)
	if err != nil {
		return fmt.Errorf("compile farm flag format: %w", err)
	}
	targets := cfg.targets(config.Teams)
	if len(targets) == 0 {
		return errors.New("farm has no targets")
	}
	cfg.logger.Printf("Starting one round for %d target(s), timeout %s per target", len(targets), options.Timeout)
	return cfg.withDelivery(func(inbox chan<- foundFlag) error {
		return cfg.runTargets(ctx, targets, options.Timeout, pattern, inbox, true)
	})
}
