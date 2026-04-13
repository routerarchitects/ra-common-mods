package apperror

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
)

// Code defines common application error codes.
type Code string

const (
	CodeNotFound       Code = "NOT_FOUND"
	CodeInvalidInput   Code = "INVALID_INPUT"
	CodeUnauthorized   Code = "UNAUTHORIZED"
	CodeForbidden      Code = "FORBIDDEN"
	CodeConflict       Code = "CONFLICT"
	CodeInternal       Code = "INTERNAL_SERVER"
	CodeNotImplemented Code = "NOT_IMPLEMENTED"
	CodeUnknown        Code = "UNKNOWN"
)

// Frame captures where the error was created.
type Frame struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// Error defines the semantic application error.
type Error struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Cause   error          `json:"-"`
	Meta    map[string]any `json:"meta,omitempty"`
	Frame   *Frame         `json:"frame,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// Unwrap allows errors.Is / errors.As to work.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New creates a new application error.
func New(code Code, message string) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Frame:   captureFrame(1),
	}
}

// Wrap creates a new application error with a cause.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Cause:   cause,
		Frame:   captureFrame(1),
	}
}

// WithMeta attaches metadata to the error.
func (e *Error) WithMeta(meta map[string]any) *Error {
	if e == nil {
		return nil
	}
	e.Meta = cloneMeta(meta)
	return e
}

// WithCause attaches a cause to the error.
func (e *Error) WithCause(cause error) *Error {
	if e == nil {
		return nil
	}
	e.Cause = cause
	return e
}

func captureFrame(skip int) *Frame {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return nil
	}

	fn := runtime.FuncForPC(pc)
	funcName := ""
	if fn != nil {
		funcName = fn.Name()
	}

	return &Frame{
		File:     filepath.Base(file),
		Line:     line,
		Function: funcName,
	}
}

// CodeOf returns the application code if present.
func CodeOf(err error) Code {
	var appErr *Error
	if errors.As(err, &appErr) && appErr != nil {
		return appErr.Code
	}
	return CodeUnknown
}

// MessageOf returns only the application message.
func MessageOf(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) && appErr != nil {
		return appErr.Message
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// LogValue makes *Error compatible with slog structured logging.
func (e *Error) LogValue() slog.Value {
	if e == nil {
		return slog.AnyValue(nil)
	}
	return slog.AnyValue(errorTreeMap(e, 0))
}

// SlogAttrs returns structured slog fields for any error.
func SlogAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	return []slog.Attr{slog.Any("error", errorTreeMap(err, 0))}
}

const maxErrorTreeDepth = 8

func errorTreeMap(err error, depth int) map[string]any {
	if err == nil {
		return nil
	}

	out := map[string]any{}

	if appErr, ok := err.(*Error); ok && appErr != nil {
		out["code"] = string(appErr.Code)
		out["message"] = appErr.Message

		if appErr.Frame != nil {
			out["frame"] = map[string]any{
				"file":     appErr.Frame.File,
				"line":     appErr.Frame.Line,
				"function": appErr.Frame.Function,
			}
		}

		if len(appErr.Meta) > 0 {
			out["meta"] = appErr.Meta
		}
	} else {
		out["code"] = string(CodeUnknown)
		out["message"] = err.Error()
		out["type"] = fmt.Sprintf("%T", err)
	}

	if depth >= maxErrorTreeDepth {
		out["truncated"] = true
		return out
	}

	causes := unwrapAll(err)
	if len(causes) == 0 {
		return out
	}

	causeTree := make([]any, 0, len(causes))
	for _, cause := range causes {
		causeMap := errorTreeMap(cause, depth+1)
		if causeMap != nil {
			causeTree = append(causeTree, causeMap)
		}
	}
	if len(causeTree) > 0 {
		out["causes"] = causeTree
	}

	return out
}

func unwrapAll(err error) []error {
	if err == nil {
		return nil
	}

	type multiUnwrapper interface {
		Unwrap() []error
	}
	if multi, ok := err.(multiUnwrapper); ok {
		return multi.Unwrap()
	}

	type singleUnwrapper interface {
		Unwrap() error
	}
	if single, ok := err.(singleUnwrapper); ok {
		cause := single.Unwrap()
		if cause != nil {
			return []error{cause}
		}
	}

	return nil
}

func cloneMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}

	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}
