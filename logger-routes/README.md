# Logger Routes Module

This module provides **Runtime Configuration APIs** for the `logger` module, integrated with the [Fiber](https://gofiber.io/) web framework.

It allows you to inspect and modify log levels (including subsystem-specific levels) dynamically at runtime without restarting the application.

## Installation

```bash
go get github.com/routerarchitects/ra-common-mods/logger-routes
```

## Usage

Register the routes with your Fiber application or router group.

```go
package main

import (
    "github.com/gofiber/fiber/v3"
    "github.com/routerarchitects/ra-common-mods/logger"
    logger_routes "github.com/routerarchitects/ra-common-mods/logger-routes"
)

func main() {
    // ... init logger ...

    app := fiber.New()

    // Register routes under a group (recommended)
    // This will create endpoints like /admin/logger/subsystems
    adminGroup := app.Group("/admin/logger")
    logger_routes.RegisterFiberRoutes(adminGroup)

    app.Listen(":8080")
}
```

## API Reference

### Get Log Levels
Returns a list of all supported log levels.

- **URL**: `/log_levels`
- **Method**: `GET`
- **Response**: `200 OK`
  ```json
  {
      "levels": ["debug", "info", "warn", "error"]
  }
  ```

### Get Subsystem Levels
Returns the current log levels for all registered subsystems.

- **URL**: `/subsystems`
- **Method**: `GET`
- **Response**: `200 OK`
  ```json
  {
      "db": "info",
      "http": "debug",
      "worker": "warn"
  }
  ```

### Update Subsystem Levels
Updates the log levels for specific subsystems.

- **URL**: `/subsystems`
- **Method**: `PUT`
- **Body**: Map of subsystem names to log levels.
  ```json
  {
      "db": "debug",
      "worker": "error"
  }
  ```
- **Response**: `200 OK` (returns the updated state of all subsystems)
  ```json
  {
      "db": "debug",
      "http": "debug",
      "worker": "error"
  }
  ```
