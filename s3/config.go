package s3

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/routerarchitects/ra-common-mods/apperror"
)

// DefaultEnvPrefix is the standard OpenWiFi prefix for S3 configuration.
const DefaultEnvPrefix = "STORAGE_TYPE_S3_"

const (
	defaultMaxConcurrentUploads = 4
	defaultPartSize             = 5 * 1024 * 1024
	maxPartSize                 = 5 * 1024 * 1024 * 1024
	defaultMaxUploadBufferBytes = 128 * 1024 * 1024
)

// Config holds S3 connection and upload settings. Relative env tags support
// applying a prefix when loading this config or embedding it in a larger one.
type Config struct {
	Endpoint             string `env:"ENDPOINT" envDefault:"https://127.0.0.1:9000"`
	AccessKey            string `env:"ACCESS_KEY"`
	SecretKey            string `env:"SECRET_KEY"`
	Region               string `env:"REGION" envDefault:"us-east-1"`
	ForcePathStyle       bool   `env:"FORCE_PATH_STYLE" envDefault:"true"`
	MaxConcurrentUploads int    `env:"MAX_CONCURRENT_UPLOADS" envDefault:"4"`
	PartSize             uint64 `env:"PART_SIZE" envDefault:"5242880"`
	MaxUploadBufferBytes uint64 `env:"MAX_UPLOAD_BUFFER_BYTES" envDefault:"134217728"`
}

// LoadConfig parses environment variables using the supplied prefix.
// An empty prefix selects DefaultEnvPrefix.
func LoadConfig(prefix string) (Config, error) {
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	var cfg Config
	if err := env.ParseWithOptions(&cfg, env.Options{Prefix: prefix}); err != nil {
		return Config{}, apperror.Wrap(apperror.CodeInvalidInput, fmt.Sprintf("failed to parse S3 config with prefix %q", prefix), err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DefaultConfig loads configuration with the standard OpenWiFi prefix.
func DefaultConfig() (Config, error) {
	return LoadConfig(DefaultEnvPrefix)
}

// Validate checks configuration fields. Zero upload settings select their defaults.
func (cfg Config) Validate() error {
	_, err := cfg.validatedEndpoint()
	return err
}

func (cfg Config) validatedEndpoint() (*url.URL, error) {
	raw := strings.TrimSpace(cfg.Endpoint)
	endpoint, err := url.Parse(raw)
	if err != nil {
		// url.Error includes the original input. Redact it before wrapping.
		if parseErr, ok := err.(*url.Error); ok {
			err = &url.Error{Op: parseErr.Op, Err: parseErr.Err}
		}
		return nil, apperror.Wrap(apperror.CodeInvalidInput, "invalid S3 endpoint URL", err)
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, apperror.New(apperror.CodeInvalidInput, "S3 endpoint requires an explicit http:// or https:// URL")
	}
	host := endpoint.Hostname()
	ip := s3utils.IsValidIP(host)
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(raw, "#") ||
		endpoint.RawPath != "" || (endpoint.Path != "" && endpoint.Path != "/") ||
		(!ip && (!s3utils.IsValidDomain(host) || strings.Contains(host, ".."))) || strings.HasSuffix(endpoint.Host, ":") ||
		(strings.Contains(host, ":") && !strings.HasPrefix(endpoint.Host, "[")) ||
		(strings.HasPrefix(endpoint.Host, "[") && !ip) {
		return nil, apperror.New(apperror.CodeInvalidInput, "S3 endpoint must contain only a valid host and optional port")
	}
	if port := endpoint.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return nil, apperror.New(apperror.CodeInvalidInput, "S3 endpoint port must be between 1 and 65535")
		}
	}
	endpoint.Path = ""

	if (strings.TrimSpace(cfg.AccessKey) == "") != (strings.TrimSpace(cfg.SecretKey) == "") {
		return nil, apperror.New(apperror.CodeInvalidInput, "s3 credentials incomplete: both access key and secret key must be provided, or both omitted for IAM authentication")
	}
	cfg = cfg.withUploadDefaults()
	if cfg.MaxConcurrentUploads < 0 {
		return nil, apperror.New(apperror.CodeInvalidInput, "max concurrent uploads must be nonnegative")
	}
	if cfg.PartSize < defaultPartSize || cfg.PartSize > maxPartSize {
		return nil, apperror.New(apperror.CodeInvalidInput, "multipart part size must be between 5 MiB and 5 GiB")
	}
	if cfg.PartSize > uint64(int(^uint(0)>>1)) {
		return nil, apperror.New(apperror.CodeInvalidInput, "multipart part size is too large for this platform")
	}
	if cfg.PartSize > cfg.MaxUploadBufferBytes/uint64(cfg.MaxConcurrentUploads) {
		return nil, apperror.New(apperror.CodeInvalidInput, "concurrent uploads and part size exceed the upload buffer budget")
	}
	return endpoint, nil
}

func (cfg Config) withUploadDefaults() Config {
	if cfg.PartSize == 0 {
		cfg.PartSize = defaultPartSize
	}
	if cfg.MaxConcurrentUploads == 0 {
		cfg.MaxConcurrentUploads = defaultMaxConcurrentUploads
	}
	if cfg.MaxUploadBufferBytes == 0 {
		cfg.MaxUploadBufferBytes = defaultMaxUploadBufferBytes
	}
	return cfg
}
