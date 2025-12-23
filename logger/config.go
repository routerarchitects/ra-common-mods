package logger

import "log/slog"

// Config is owned by this module. Services provide an initial Config at Init,
// and may call Update with a new Config at runtime.
//
// It is intended to be populated via github.com/caarlos0/env parsing in services.
type Config struct {
	ServiceName    string `env:"SERVICE_NAME,required"`
	ServiceVersion string `env:"SERVICE_VERSION,,required"`
	Environment    string `env:"ENVIRONMENT" envDefault:"dev"`

	Output OutputConfig `envPrefix:"LOG_"`

	Levels LevelsConfig `envPrefix:"LOG_"`

	Correlation CorrelationConfig `envPrefix:"LOG_CORR_"`

	Redaction RedactionConfig `envPrefix:"LOG_REDACT_"`

	Stacktrace StacktraceConfig `envPrefix:"LOG_STACK_"`
}

type OutputConfig struct {
	// Format: "json" or "text"
	// If empty, defaults to:
	// - dev: text
	// - otherwise: json
	Format string `env:"FORMAT" envDefault:"json"`

	// AddSource: include file:line
	AddSource bool `env:"ADD_SOURCE" envDefault:"false"`
}

type LevelsConfig struct {
	// DefaultLevel applies when subsystem is not in SubsystemLevels.
	DefaultLevel string `env:"LEVEL" envDefault:"info"`

	// SubsystemLevelsRaw is the env-friendly encoding of subsystem levels.
	// Example: "http=info,db=warn,worker=debug"
	SubsystemLevelsRaw string `env:"SUBSYSTEM_LEVELS" envDefault:""`

	// SubsystemLevels is the parsed map (owned by module at runtime).
	// Services may leave this empty and rely on SubsystemLevelsRaw.
	SubsystemLevels map[string]slog.Level `env:"-"`
}

type CorrelationConfig struct {
	Enabled bool `env:"ENABLED" envDefault:"true"`

	// Header names to read/write correlation IDs.
	RequestIDHeader string `env:"REQUEST_ID_HEADER" envDefault:"X-Request-Id"`
	TraceIDHeader   string `env:"TRACE_ID_HEADER" envDefault:"X-Trace-Id"`
	SpanIDHeader    string `env:"SPAN_ID_HEADER" envDefault:"X-Span-Id"`

	GenerateRequestID bool `env:"GENERATE_REQUEST_ID" envDefault:"true"`
	Propagate         bool `env:"PROPAGATE" envDefault:"true"`
}

type RedactionConfig struct {
	Enabled bool `env:"ENABLED" envDefault:"false"`

	// KeysCSV is a denylist of keys whose values must be replaced.
	// If empty, module uses defaults internally.
	KeysCSV string `env:"KEYS" envDefault:"authorization,cookie,set-cookie,password,passwd,token,access_token,refresh_token,secret,api_key,x-api-key"`

	Replacement string `env:"REPLACEMENT" envDefault:"******"`
}

type StacktraceConfig struct {
	Enabled bool `env:"ENABLED" envDefault:"false"`

	// Level: include stacktrace for logs at/above this level (e.g. "error")
	Level string `env:"LEVEL" envDefault:"error"`
}
