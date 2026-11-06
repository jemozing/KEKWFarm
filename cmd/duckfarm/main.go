package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"duckfarm/internal/farm"
	"duckfarm/internal/runner"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	const usage = "Usage: duckfarm serve [-config FILE] | run [options] EXPLOIT | all [-config FILE] -- [options] EXPLOIT"
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(os.Stdout, usage)
		return nil
	}
	mode := args[0]
	switch mode {
	case "run":
		return runner.Run(ctx, args[1:], runner.Defaults{})
	case "serve", "all":
	default:
		return errors.New(usage)
	}
	options := flag.NewFlagSet(mode, flag.ContinueOnError)
	path := options.String("config", "duckfarm.json", "path to the farm configuration")
	if err := options.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if (mode == "serve" && options.NArg() != 0) || (mode == "all" && options.NArg() == 0) {
		return errors.New(usage)
	}
	cfg, err := farm.Load(*path)
	if err != nil {
		return err
	}
	if mode == "serve" {
		return farm.Run(ctx, cfg, nil)
	}
	return runAll(ctx, cfg, options.Args())
}

func runAll(ctx context.Context, cfg farm.Config, args []string) error {
	ctx, stopServer := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServer()
	ready := make(chan net.Addr, 1)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- farm.Run(ctx, cfg, func(address net.Addr) { ready <- address })
	}()

	var address net.Addr
	select {
	case address = <-ready:
	case err := <-serverDone:
		return err
	case <-ctx.Done():
		stopServer()
		return <-serverDone
	}
	host, port, err := net.SplitHostPort(address.String())
	if err != nil {
		stopServer()
		<-serverDone
		return err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}

	runnerContext, stopRunner := context.WithCancel(context.Background())
	defer stopRunner()
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(runnerContext, args, runner.Defaults{
			URL: "http://" + net.JoinHostPort(host, port), Password: cfg.Password,
		})
	}()
	select {
	case err := <-runnerDone:
		if err != nil {
			stopServer()
			return errors.Join(err, <-serverDone)
		}
		select {
		case <-ctx.Done():
			stopServer()
			return <-serverDone
		case err := <-serverDone:
			return err
		}
	case err := <-serverDone:
		stopRunner()
		return errors.Join(err, <-runnerDone)
	case <-ctx.Done():
		stopRunner()
		err := <-runnerDone
		stopServer()
		return errors.Join(err, <-serverDone)
	}
}
