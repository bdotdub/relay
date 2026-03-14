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

	app, err := newRelayApp(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if err := app.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
