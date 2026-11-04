package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte("api:\n  base_url: https://api.upsignal.app\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.API.BaseURL = "https://api.upsignal.app"
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestParseFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
api:
  base_url: "https://api.example.com"
  request_timeout: 5s
collection:
  interval: 15s
  upload_interval: 2m
spool:
  enabled: false
  max_age: 12h
  max_size: 10MB
logging:
  level: debug
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.Collection.UploadInterval); got != 2*time.Minute {
		t.Errorf("upload_interval = %s", got)
	}
	if cfg.Spool.Enabled || cfg.Spool.MaxSize != 10*MiB || cfg.Logging.Level != "debug" {
		t.Errorf("unexpected spool/logging: %+v %+v", cfg.Spool, cfg.Logging)
	}
}

func TestParseRejects(t *testing.T) {
	tests := map[string]struct{ yaml, want string }{
		"missing base_url": {"", "api.base_url is required"},
		"plain http":       {"api: {base_url: http://api.example.com}", "must use https"},
		"credentials":      {"api: {base_url: https://u:p@api.example.com}", "must not contain credentials"},
		"query":            {"api: {base_url: \"https://api.example.com/?t=x\"}", "query"},
		"unknown field":    {"api: {base_url: https://a.example.com, verify_tls: false}", "field verify_tls not found"},
		"bad duration":     {"api: {base_url: https://a.example.com}\ncollection: {interval: soon}", "invalid duration"},
		"interval too low": {"api: {base_url: https://a.example.com}\ncollection: {interval: 1s}", "collection.interval"},
		"upload < collect": {"api: {base_url: https://a.example.com}\ncollection: {interval: 60s, upload_interval: 30s}", "at least collection.interval"},
		"bad size":         {"api: {base_url: https://a.example.com}\nspool: {max_size: lots}", "invalid size"},
		"size too big":     {"api: {base_url: https://a.example.com}\nspool: {max_size: 2GB}", "spool.max_size"},
		"bad level":        {"api: {base_url: https://a.example.com}\nlogging: {level: trace}", "logging.level"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	tests := map[string]ByteSize{"50MB": 50 * MiB, "50MiB": 50 * MiB, "1GB": GiB, "512 KB": 512 * KiB, "100B": 100}
	for in, want := range tests {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1MB", "1.5MB", "10TB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) succeeded, want error", in)
		}
	}
}
