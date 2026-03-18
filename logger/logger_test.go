package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestLoggerFlow(t *testing.T) {

	// Ensure global config is initialized for test
	globalConfig.Store(Config{
		Levels: LevelsConfig{
			SubsystemLevels: map[string]slog.Level{},
		},
	})
	// 2. Redirect output to buffer
	var buf bytes.Buffer

	// We need to hijack the output writer.
	// Our Init hardcodes os.Stderr.
	// For testing, we might need to expose an option or just trust Init works and unit test Handlers?
	// OR we can just modify Init to accept an io.Writer option?
	// Or we use the public handlers directly.

	// Let's test the Handlers directly to verify logic without changing Init yet.
	// We reconstruct the chain manually for test.

	// Redaction function
	replacer := redactionFunction(map[string]struct{}{"password": {}}, "[REDACTED]")

	jsonH := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:       slog.LevelDebug, // Backend allows everything
		ReplaceAttr: replacer,
	})

	// Subsystem Handler
	subH := &SubsystemHandler{
		Next:          jsonH,
		subSystemName: "test",
		defaultLevel:  slog.LevelInfo,
	}

	// Context Handler
	ctxH := &ContextHandler{Next: subH}

	l := slog.New(ctxH)
	l = l.With("service", "test")

	// 3. Test Cases
	ctx := context.Background()

	// A. Info Log (Should show)
	l.InfoContext(ctx, "hello world", "password", "secret123")

	// Verify JSON
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Failed to parse json: %v", err)
	}

	if entry["msg"] != "hello world" {
		t.Errorf("Expected msg 'hello world', got %v", entry["msg"])
	}
	if entry["password"] != "[REDACTED]" {
		t.Errorf("Expected redacted password, got %v", entry["password"])
	}
	buf.Reset()

	// B. Debug Log (Default level is Info, should NOT show)
	l.DebugContext(ctx, "debug message")
	if buf.Len() > 0 {
		t.Error("Expected debug message to be filtered out")
	}
	buf.Reset()

	// C. Subsystem Log (DB=debug, should show)
	// We must register the "db" subsystem with Debug level for it to show
	if err := UpdateSubsystemLevels(map[string]string{"db": "debug"}); err != nil {
		t.Fatalf("expected subsystem update to succeed, got error: %v", err)
	}

	dbLog := l.With("subsystem", "db")
	dbLog.DebugContext(ctx, "db query")
	if buf.Len() == 0 {
		t.Error("Expected db debug message to show")
	}
	buf.Reset()
}

func TestGetCopyConfigReturnsCopy(t *testing.T) {
	globalConfig.Store(Config{
		Levels: LevelsConfig{
			DefaultLevel:    "info",
			SubsystemLevels: map[string]slog.Level{"db": slog.LevelDebug},
		},
	})

	cfg := GetCopyConfig()
	cfg.Levels.SubsystemLevels["db"] = slog.LevelError
	cfg.Levels.SubsystemLevels["new"] = slog.LevelWarn

	fresh := getConfig()
	if fresh.Levels.SubsystemLevels["db"] != slog.LevelDebug {
		t.Fatalf("expected internal config to stay unchanged, got db=%s", fresh.Levels.SubsystemLevels["db"])
	}
	if _, ok := fresh.Levels.SubsystemLevels["new"]; ok {
		t.Fatal("expected mutation on returned config map to not leak into global state")
	}
}

func TestUpdateSubsystemLevelsRejectsInvalidLevel(t *testing.T) {
	globalConfig.Store(Config{
		Levels: LevelsConfig{
			DefaultLevel:    "info",
			SubsystemLevels: map[string]slog.Level{"db": slog.LevelDebug},
		},
	})

	if err := UpdateSubsystemLevels(map[string]string{"db": "not-a-level"}); err == nil {
		t.Fatal("expected invalid subsystem level to return an error")
	}

	cfg := getConfig()
	if cfg.Levels.SubsystemLevels["db"] != slog.LevelDebug {
		t.Fatalf("expected db level to remain debug after failed update, got %s", cfg.Levels.SubsystemLevels["db"])
	}
}

func TestInitRejectsInvalidDefaultLevel(t *testing.T) {
	_, _, err := Init(Config{
		ServiceName:    "svc",
		ServiceVersion: "1.0.0",
		Environment:    "dev",
		Levels: LevelsConfig{
			DefaultLevel: "not-a-level",
		},
	})
	if err == nil {
		t.Fatal("expected invalid default log level to fail Init")
	}
}
