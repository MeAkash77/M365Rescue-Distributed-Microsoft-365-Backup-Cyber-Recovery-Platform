package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"m365rescue/internal/api"
	"m365rescue/internal/app"
	"m365rescue/internal/config"
	"m365rescue/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: m365rescue <api|worker>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config.Load()
	d, err := app.Build(ctx, cfg)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	switch os.Args[1] {
	case "api":
		err = api.Run(ctx, d)
	case "worker":
		err = worker.Run(ctx, d)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
