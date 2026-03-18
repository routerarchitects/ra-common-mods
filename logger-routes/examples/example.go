package main

import (
	"context"
	"time"

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
	defer shutdown() // No arguments

	root.InfoContext(context.Background(), "starting service", "password", "1234512q2323213")

	subSysLog := logger.Subsystem("test")
	subSysLog.InfoContext(context.Background(), "subsys log")

	go func() {
		for {
			time.Sleep(time.Second)
			subSysLog.InfoContext(context.Background(), "subsys log")
			root.InfoContext(context.Background(), "root log")
			root.ErrorContext(context.Background(), "root error")
			subSysLog.ErrorContext(context.Background(), "subsys error")
		}
	}()

	app := fiber.New()
	logger_routes.RegisterFiberRoutes(app.Group("/logger"))
	app.Listen(":8080")
}
