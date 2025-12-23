package main

import (
	"context"

	"github.com/caarlos0/env/v11"
	"github.com/gofiber/fiber/v3"

	"github.com/routerarchitects/ra-common-mods/logger"
)

type Config struct {
	Port string `env:"PORT" envDefault:"8080"`
	Log  logger.Config
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

	app := fiber.New()
	app.Use(logger.FiberMiddleware())

	// Helpers that return *slog.Logger with pre-bound subsystem attribute
	httpLog := logger.Subsystem("http")
	dbLog := logger.Subsystem("db")

	app.Get("/health", func(c fiber.Ctx) error {
		ctx := c.Context()
		// New slog usage: method chaining with With()
		httpLog.With("path", c.Path()).InfoContext(ctx, "health ok")
		return c.SendString("ok")
	})

	app.Get("/db", func(c fiber.Ctx) error {
		ctx := c.Context()

		err := fakeDBCall()
		if err != nil {
			dbLog.With(
				"op", "fakeDBCall",
				"error", err,
			).ErrorContext(ctx, "db failed")

			return fiber.NewError(fiber.StatusInternalServerError, "db failed")
		}

		// levels, _ := logger.GetSubsystemLevels()
		dbLog.InfoContext(ctx, "db api gets called")
		dbLog.ErrorContext(ctx, "db api gets called")

		return c.JSON(fiber.Map{"ok": true})
	})

	logger.RegisterFiberRoutes(app.Group("/logger"))

	root.InfoContext(context.Background(), "starting service", "password", "1234512q2323213")

	if err := app.Listen(":" + cfg.Port); err != nil {
		root.With("error", err).ErrorContext(context.Background(), "listen failed")
	}
}

func fakeDBCall() error {
	return nil
}
