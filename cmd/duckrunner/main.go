package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"duckfarm/internal/runner"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runner.Run(ctx, os.Args[1:], runner.Defaults{}); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
