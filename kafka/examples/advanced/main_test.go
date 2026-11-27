package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/routerarchitects/ra-common-mods/kafka"
)

type consumerRunResult struct {
	kafka.Consumer
	err error
}

func (c consumerRunResult) Run(context.Context) error { return c.err }

func TestConsumerRunFailureCancelsScenarios(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			report := newScenarioReport(cancel)
			runErr := fmt.Errorf("consumer failed: %w", err)
			report.runConsumer(ctx, consumerRunResult{err: runErr}, logger, "dlq")
			if ctx.Err() == nil {
				t.Fatal("consumer failure did not cancel the other scenarios")
			}
			assertScenario(logger, report, "dlq", true, "message received")
			if result := report.results["dlq"]; result.Passed || result.Reason != runErr.Error() {
				t.Fatalf("expected the scenario to retain the Run failure, got %+v", result)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	report := newScenarioReport(cancel)
	report.runConsumer(ctx, consumerRunResult{}, logger, "dlq")
	if ctx.Err() != nil || len(report.results) != 0 {
		t.Fatal("a successful Run should leave the other scenarios running")
	}
}
