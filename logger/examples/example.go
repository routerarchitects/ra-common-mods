package main

import (
	"context"

	"github.com/caarlos0/env/v11"
	"github.com/routerarchitects/ra-common-mods/logger"
)

type Config struct {
	Log logger.Config
}

func main() {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		panic(err)
	}

	root, shutdown, err := logger.Init(cfg.Log)
	if err != nil {
		panic(err)
	}
	defer shutdown() // No arguments

	root.InfoContext(context.Background(), "starting service", "password", "1234512q2323213")
}
