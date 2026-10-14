package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/config"
)

func runBuildWorker(args []string, cfg config.Config) {
	if len(args) != 1 {
		log.Fatalf("build-worker requires exactly one build ID")
	}
	if err := builder.RunWorker(cfg.BuildDir, cfg.NixpacksPath, args[0]); err != nil {
		log.Fatalf("build worker failed: %v", err)
	}
}

func runBuildBroker() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := builder.RunBroker(ctx, builder.BrokerSocket)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("build broker failed: %v", err)
	}
}
