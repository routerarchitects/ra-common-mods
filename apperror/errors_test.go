package apperror

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorStringAndUnwrap(t *testing.T) {
	var nilErr *Error
	if got := nilErr.Error(); got != "" {
		t.Fatalf("expected empty string for nil *Error, got %q", got)
	}
	if got := nilErr.Unwrap(); got != nil {
		t.Fatalf("expected nil unwrap for nil *Error, got %v", got)
	}

	root := errors.New("db timeout")
	err := Wrap(CodeInternal, "query failed", root)

	if got := err.Error(); got != "[INTERNAL] query failed: db timeout" {
		t.Fatalf("unexpected error string: %q", got)
	}
	if !errors.Is(err, root) {
		t.Fatal("expected wrapped error to match root cause via errors.Is")
	}
}

func TestNewAndWrapFrames(t *testing.T) {
	e1 := New(CodeConflict, "already exists")
	if e1.Frame() == nil {
		t.Fatal("expected New to capture frame")
	}
	if e1.Frame().File != "errors_test.go" {
		t.Fatalf("expected New frame file to be errors_test.go, got %q", e1.Frame().File)
	}
	if !strings.Contains(e1.Frame().Function, "TestNewAndWrapFrames") {
		t.Fatalf("expected New frame function to contain test name, got %q", e1.Frame().Function)
	}

	root := errors.New("missing id")
	e2 := Wrap(CodeInvalidInput, "invalid request", root)
	if e2.Frame() == nil {
		t.Fatal("expected Wrap to capture caller frame")
	}
	if e2.Frame().File != "errors_test.go" {
		t.Fatalf("expected Wrap frame file to be errors_test.go, got %q", e2.Frame().File)
	}
	if !strings.Contains(e2.Frame().Function, "TestNewAndWrapFrames") {
		t.Fatalf("expected Wrap frame function to contain test name, got %q", e2.Frame().Function)
	}
}

func TestWrapWithMetaClonesInput(t *testing.T) {
	meta := map[string]any{"field": "id", "count": 1}
	err := WrapWithMeta(CodeInvalidInput, "bad request", nil, meta)

	meta["field"] = "name"
	meta["new"] = "value"

	errMeta := err.Meta()
	if got := errMeta["field"]; got != "id" {
		t.Fatalf("expected cloned meta field to stay id, got %v", got)
	}
	if _, ok := errMeta["new"]; ok {
		t.Fatal("expected cloned meta to not include later map mutations")
	}
}

func TestMetaGetterReturnsCopy(t *testing.T) {
	err := WrapWithMeta(CodeInvalidInput, "bad request", nil, map[string]any{"field": "a"})

	meta := err.Meta()
	meta["field"] = "changed"
	meta["new"] = "value"

	refetched := err.Meta()
	if refetched["field"] != "a" {
		t.Fatalf("expected internal meta field to stay a, got %v", refetched["field"])
	}
	if _, ok := refetched["new"]; ok {
		t.Fatal("expected internal meta to not include caller-added key")
	}
}

func TestCodeOfAndMessageOf(t *testing.T) {
	root := errors.New("transport closed")
	wrapped := Wrap(CodeInternal, "rpc failed", root)

	if got := CodeOf(wrapped); got != CodeInternal {
		t.Fatalf("expected code %s, got %s", CodeInternal, got)
	}
	if got := MessageOf(wrapped); got != "rpc failed" {
		t.Fatalf("expected message rpc failed, got %q", got)
	}

	if got := CodeOf(root); got != CodeUnknown {
		t.Fatalf("expected unknown code for plain error, got %s", got)
	}
	if got := MessageOf(root); got != "transport closed" {
		t.Fatalf("expected plain error message, got %q", got)
	}
	if got := MessageOf(nil); got != "" {
		t.Fatalf("expected empty message for nil error, got %q", got)
	}
}

func TestLogValueAndSlogAttrs(t *testing.T) {
	root := errors.New("id is empty")
	err := WrapWithMeta(CodeInvalidInput, "validation failed", root, map[string]any{"field": "id"})

	logMap, ok := err.LogValue().Any().(map[string]any)
	if !ok {
		t.Fatal("expected LogValue to expose map[string]any")
	}

	if got := logMap["code"]; got != string(CodeInvalidInput) {
		t.Fatalf("expected code %s, got %v", CodeInvalidInput, got)
	}
	if got := logMap["message"]; got != "validation failed" {
		t.Fatalf("expected message validation failed, got %v", got)
	}
	if _, ok := logMap["frame"].(map[string]any); !ok {
		t.Fatalf("expected frame map, got %T", logMap["frame"])
	}
	meta, ok := logMap["meta"].(map[string]any)
	if !ok || meta["field"] != "id" {
		t.Fatalf("expected meta field=id, got %#v", logMap["meta"])
	}

	causes, ok := logMap["causes"].([]any)
	if !ok || len(causes) != 1 {
		t.Fatalf("expected one cause, got %#v", logMap["causes"])
	}
	cause, ok := causes[0].(map[string]any)
	if !ok {
		t.Fatalf("expected cause map, got %T", causes[0])
	}
	if _, ok := cause["code"]; ok {
		t.Fatalf("expected no code for plain cause, got %v", cause["code"])
	}

	attrs := SlogAttrs(err)
	if len(attrs) != 1 || attrs[0].Key != "error" {
		t.Fatalf("expected one slog attr with key error, got %#v", attrs)
	}
	if got := SlogAttrs(nil); got != nil {
		t.Fatalf("expected nil attrs for nil error, got %#v", got)
	}
}

func TestFrameGetterReturnsCopy(t *testing.T) {
	err := New(CodeInternal, "boom")

	frame := err.Frame()
	if frame == nil {
		t.Fatal("expected frame")
	}
	origFile := frame.File
	frame.File = "mutated.go"

	refetched := err.Frame()
	if refetched == nil {
		t.Fatal("expected refetched frame")
	}
	if refetched.File != origFile {
		t.Fatalf("expected frame file to remain %q, got %q", origFile, refetched.File)
	}
}

func TestSlogAttrsWithJoinedErrors(t *testing.T) {
	joined := errors.Join(errors.New("a"), errors.New("b"))
	attrs := SlogAttrs(joined)
	if len(attrs) != 1 {
		t.Fatalf("expected one attr for joined errors, got %d", len(attrs))
	}
	m, ok := attrs[0].Value.Any().(map[string]any)
	if !ok {
		t.Fatalf("expected attr value as map, got %T", attrs[0].Value.Any())
	}
	causes, ok := m["causes"].([]any)
	if !ok || len(causes) != 2 {
		t.Fatalf("expected two causes for errors.Join, got %#v", m["causes"])
	}
}
