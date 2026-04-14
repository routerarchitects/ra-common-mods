package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/routerarchitects/ra-common-mods/apperror"
)

func main() {
	rootCause := errors.New("id is empty")

	err := apperror.Wrap(apperror.CodeInvalidInput, "request validation failed", rootCause).WithMeta(map[string]any{
		"id": "test_id",
	})

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	log.Error("Processing error", "error", err)
}
