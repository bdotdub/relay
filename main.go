package main

import (
	"context"
	"log"
)

func main() {
	cfg, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}
	setVerboseLogging(cfg.verbose)
	verbosef("starting relay %s", kvSummary(
		"start_app_server", cfg.codexStartAppServer,
		"cwd", cfg.codexCWD,
		"state_path", cfg.statePath,
	))

	app, err := newRelayApp(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if err := app.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
