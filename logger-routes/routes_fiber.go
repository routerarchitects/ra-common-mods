package logger_routes_fiber

import (
	"github.com/gofiber/fiber/v3"
	"github.com/routerarchitects/ra-common-mods/logger"
)

// RegisterFiberRoutes registers the logger configuration routes.
func RegisterFiberRoutes(r fiber.Router) {
	r.Get("/subsystems", getSubsystemLevelsHandler)
	r.Put("/subsystems", updateSubsystemLevelsHandler)
	r.Get("/log_levels", getLogLevelsHandler)
}

func getSubsystemLevelsHandler(c fiber.Ctx) error {
	levels, err := logger.GetSubsystemLevels()
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(levels)
}

func updateSubsystemLevelsHandler(c fiber.Ctx) error {
	var newLevels map[string]string
	if err := c.Bind().Body(&newLevels); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid payload")
	}
	if err := logger.UpdateSubsystemLevels(newLevels); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	levels, _ := logger.GetSubsystemLevels()
	return c.JSON(levels)
}

func getLogLevelsHandler(c fiber.Ctx) error {
	levels := logger.GetAllLevels()
	return c.JSON(fiber.Map{
		"levels": levels,
	})
}
