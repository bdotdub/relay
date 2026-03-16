package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bdotdub/relay/internal/config"
	"github.com/bdotdub/relay/internal/logx"
	"github.com/bdotdub/relay/internal/relay"
)

func main() {
	cfg, err := config.Parse()
	if err != nil {
		log.Fatal(err)
	}
	if err := logx.SetLevel(cfg.LogLevel); err != nil {
		log.Fatal(err)
	}
	logx.Infof("starting relay %s", logx.KVSummary(
		"log_level", cfg.LogLevel,
		"start_app_server", cfg.CodexStartAppServer,
		"cwd", cfg.CodexCWD,
		"state_path", cfg.StatePath,
	))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := relay.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}

	logx.Infof("relay stopped")
}
