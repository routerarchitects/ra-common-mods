package main

import (
	"github.com/caarlos0/env/v11"
	"github.com/gofiber/fiber/v3"
	"github.com/routerarchitects/ra-common-mods/logger"
	logger_routes "github.com/routerarchitects/ra-common-mods/logger-routes"
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
	defer shutdown()

	root.Info("Logger initialized")

	app := fiber.New()
	logger_routes.RegisterFiberRoutes(app.Group("/logger"))
	app.Listen(":8080")
}
