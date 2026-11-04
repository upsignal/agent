// Package config loads and validates the agent's non-secret configuration.
// Credentials never live here; they are stored separately in the state directory.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultPath     = "/etc/upsignal-agent/config.yaml"
	DefaultStateDir = "/var/lib/upsignal-agent"
)

type Config struct {
	API        API        `yaml:"api"`
	Collection Collection `yaml:"collection"`
	Spool      Spool      `yaml:"spool"`
	Logging    Logging    `yaml:"logging"`
}

type API struct {
	BaseURL        string   `yaml:"base_url"`
	RequestTimeout Duration `yaml:"request_timeout"`
}

type Collection struct {
	Interval       Duration `yaml:"interval"`
	UploadInterval Duration `yaml:"upload_interval"`
}

type Spool struct {
	Enabled bool     `yaml:"enabled"`
	MaxAge  Duration `yaml:"max_age"`
	MaxSize ByteSize `yaml:"max_size"`
}

type Logging struct {
	Level string `yaml:"level"`
}

// Default returns the spec defaults. BaseURL is intentionally empty: it must come from the config file.
func Default() Config {
	return Config{
		API:        API{RequestTimeout: Duration(10 * time.Second)},
		Collection: Collection{Interval: Duration(30 * time.Second), UploadInterval: Duration(60 * time.Second)},
		Spool:      Spool{Enabled: true, MaxAge: Duration(24 * time.Hour), MaxSize: 50 * MiB},
		Logging:    Logging{Level: "info"},
	}
}

// Load reads path, applies defaults for omitted fields, and validates the result.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Reject typos such as `upload_intervall` instead of silently using the default.
	dec.KnownFields(true)
	// An empty document means "all defaults"; validation still requires api.base_url.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if err := validateBaseURL(c.API.BaseURL); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs,
		inRange("api.request_timeout", c.API.RequestTimeout, time.Second, time.Minute),
		inRange("collection.interval", c.Collection.Interval, 10*time.Second, 10*time.Minute),
		inRange("collection.upload_interval", c.Collection.UploadInterval, 10*time.Second, time.Hour),
		inRange("spool.max_age", c.Spool.MaxAge, time.Minute, 7*24*time.Hour),
	)
	if c.Collection.UploadInterval < c.Collection.Interval {
		errs = append(errs, errors.New("collection.upload_interval must be at least collection.interval"))
	}
	if c.Spool.MaxSize < MiB || c.Spool.MaxSize > GiB {
		errs = append(errs, fmt.Errorf("spool.max_size must be between 1MB and 1GB, got %s", c.Spool.MaxSize))
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("logging.level must be debug, info, warn or error, got %q", c.Logging.Level))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}

// validateBaseURL enforces HTTPS. There is deliberately no option to allow plain HTTP or skip TLS verification.
func validateBaseURL(raw string) error {
	if raw == "" {
		return errors.New("api.base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("api.base_url: %w", err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("api.base_url must use https, got %q", u.Scheme)
	case u.Host == "":
		return errors.New("api.base_url must include a host")
	case u.User != nil:
		return errors.New("api.base_url must not contain credentials")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("api.base_url must not contain a query or fragment")
	}
	return nil
}

func inRange(name string, d Duration, lo, hi time.Duration) error {
	if v := time.Duration(d); v < lo || v > hi {
		return fmt.Errorf("%s must be between %s and %s, got %s", name, lo, hi, v)
	}
	return nil
}

// Duration accepts Go duration strings such as "30s" or "24h".
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// ByteSize accepts sizes such as "50MB". KB/MB/GB are binary (1024-based), same as KiB/MiB/GiB.
type ByteSize int64

const (
	KiB ByteSize = 1 << 10
	MiB ByteSize = 1 << 20
	GiB ByteSize = 1 << 30
)

func (b ByteSize) String() string {
	switch {
	case b >= GiB && b%GiB == 0:
		return fmt.Sprintf("%dGB", b/GiB)
	case b >= MiB && b%MiB == 0:
		return fmt.Sprintf("%dMB", b/MiB)
	case b >= KiB && b%KiB == 0:
		return fmt.Sprintf("%dKB", b/KiB)
	}
	return fmt.Sprintf("%dB", int64(b))
}

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   ByteSize
	}{
		{"KiB", KiB}, {"MiB", MiB}, {"GiB", GiB},
		{"KB", KiB}, {"MB", MiB}, {"GB", GiB},
		{"B", 1},
	}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil || n < 0 {
				break
			}
			return ByteSize(n) * u.mult, nil
		}
	}
	return 0, fmt.Errorf("invalid size %q (use e.g. 50MB)", s)
}
