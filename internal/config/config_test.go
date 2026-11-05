package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	got, err := load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got != Default() {
		t.Errorf("load(empty) = %+v, want %+v", got, Default())
	}
}

func TestHostKeyFollowsDataDir(t *testing.T) {
	env := map[string]string{"WHERRY_DATA_DIR": "/var/lib/wherry"}
	got, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if want := "/var/lib/wherry/host_ed25519"; got.SSHHostKey != want {
		t.Errorf("SSHHostKey = %q, want %q", got.SSHHostKey, want)
	}
}

func TestOverrides(t *testing.T) {
	env := map[string]string{
		"WHERRY_DATA_DIR":          "/srv/wherry",
		"WHERRY_DOMAIN":            "example.test",
		"WHERRY_HTTP_ADDR":         ":80",
		"WHERRY_HTTPS_ADDR":        ":443",
		"WHERRY_ACME_EMAIL":        "ops@example.test",
		"WHERRY_DASHBOARD_HOST":    "example.test",
		"WHERRY_SSH_ADDR":          ":22",
		"WHERRY_SSH_HOST_KEY":      "/etc/wherry/key",
		"WHERRY_INTERNAL_ADDR":     "127.0.0.1:9001",
		"WHERRY_RUNTIME":           "native",
		"WHERRY_CGROUP_ROOT":       "/sys/fs/cgroup/system.slice/wherryd.service/instances",
		"WHERRY_DOCKER_HOST":       "unix:///run/docker.sock",
		"WHERRY_BUILD_CONCURRENCY": "4",
		"WHERRY_BUILD_TIMEOUT":     "20m",
		"WHERRY_HEALTH_TIMEOUT":    "1m",
		"WHERRY_DRAIN_GRACE":       "2s",
		"WHERRY_STOP_GRACE":        "15s",
		"WHERRY_RATE_LIMIT_RPS":    "0",
		"WHERRY_LOG_LEVEL":         "debug",
		"WHERRY_LOG_FORMAT":        "text",
	}
	want := Config{
		DataDir:          "/srv/wherry",
		Domain:           "example.test",
		HTTPAddr:         ":80",
		HTTPSAddr:        ":443",
		ACMEEmail:        "ops@example.test",
		DashboardHost:    "example.test",
		SSHAddr:          ":22",
		SSHHostKey:       "/etc/wherry/key",
		InternalAddr:     "127.0.0.1:9001",
		Runtime:          RuntimeNative,
		CgroupRoot:       "/sys/fs/cgroup/system.slice/wherryd.service/instances",
		DockerHost:       "unix:///run/docker.sock",
		BuildConcurrency: 4,
		BuildTimeout:     20 * time.Minute,
		HealthTimeout:    time.Minute,
		DrainGrace:       2 * time.Second,
		StopGrace:        15 * time.Second,
		RateLimitRPS:     0,
		LogLevel:         slog.LevelDebug,
		LogFormat:        LogFormatText,
	}
	got, err := load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("load = %+v\nwant %+v", got, want)
	}
}

func TestDashboardHostValid(t *testing.T) {
	env := map[string]string{
		"WHERRY_DOMAIN":     "apps.example.test",
		"WHERRY_HTTPS_ADDR": ":443",
		"WHERRY_ACME_EMAIL": "ops@example.test",
	}
	for _, host := range []string{"example.test", "dash.example.test", "my-dash.example.test", "apps.example.test"} {
		env["WHERRY_DASHBOARD_HOST"] = host
		got, err := load(func(k string) string { return env[k] })
		if err != nil {
			t.Errorf("%s: %v", host, err)
		} else if got.DashboardHost != host {
			t.Errorf("DashboardHost = %q, want %q", got.DashboardHost, host)
		}
	}
}

func TestInvalid(t *testing.T) {
	dash := func(host string) map[string]string {
		return map[string]string{
			"WHERRY_DOMAIN":         "Apps.Example.Test",
			"WHERRY_HTTPS_ADDR":     ":443",
			"WHERRY_ACME_EMAIL":     "ops@example.test",
			"WHERRY_DASHBOARD_HOST": host,
		}
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"duration", map[string]string{"WHERRY_BUILD_TIMEOUT": "10 minutes"}, "WHERRY_BUILD_TIMEOUT"},
		{"int", map[string]string{"WHERRY_BUILD_CONCURRENCY": "two"}, "WHERRY_BUILD_CONCURRENCY"},
		{"addr without port", map[string]string{"WHERRY_HTTP_ADDR": "8080"}, "WHERRY_HTTP_ADDR"},
		{"addr with empty port", map[string]string{"WHERRY_SSH_ADDR": "localhost:"}, "WHERRY_SSH_ADDR"},
		{"concurrency zero", map[string]string{"WHERRY_BUILD_CONCURRENCY": "0"}, "WHERRY_BUILD_CONCURRENCY"},
		{"negative rps", map[string]string{"WHERRY_RATE_LIMIT_RPS": "-1"}, "WHERRY_RATE_LIMIT_RPS"},
		{"negative duration", map[string]string{"WHERRY_STOP_GRACE": "-5s"}, "WHERRY_STOP_GRACE"},
		{"runtime", map[string]string{"WHERRY_RUNTIME": "podman"}, "WHERRY_RUNTIME"},
		{"cgroup root", map[string]string{"WHERRY_CGROUP_ROOT": "relative/path"}, "WHERRY_CGROUP_ROOT"},
		{"log level", map[string]string{"WHERRY_LOG_LEVEL": "loud"}, "WHERRY_LOG_LEVEL"},
		{"log format", map[string]string{"WHERRY_LOG_FORMAT": "yaml"}, "WHERRY_LOG_FORMAT"},
		{"https without email", map[string]string{"WHERRY_HTTPS_ADDR": ":443"}, "WHERRY_ACME_EMAIL"},
		{"https addr", map[string]string{"WHERRY_HTTPS_ADDR": "443", "WHERRY_ACME_EMAIL": "a@b.c"}, "WHERRY_HTTPS_ADDR"},
		{"internal addr", map[string]string{"WHERRY_INTERNAL_ADDR": "9000"}, "WHERRY_INTERNAL_ADDR"},
		{"rps not int", map[string]string{"WHERRY_RATE_LIMIT_RPS": "many"}, "WHERRY_RATE_LIMIT_RPS"},
		{"health not duration", map[string]string{"WHERRY_HEALTH_TIMEOUT": "1 hour"}, "WHERRY_HEALTH_TIMEOUT"},
		{"drain not duration", map[string]string{"WHERRY_DRAIN_GRACE": "soon"}, "WHERRY_DRAIN_GRACE"},
		{"stop not duration", map[string]string{"WHERRY_STOP_GRACE": "later"}, "WHERRY_STOP_GRACE"},
		{"build timeout zero", map[string]string{"WHERRY_BUILD_TIMEOUT": "0"}, "WHERRY_BUILD_TIMEOUT"},
		{"health negative", map[string]string{"WHERRY_HEALTH_TIMEOUT": "-30s"}, "WHERRY_HEALTH_TIMEOUT"},
		{"drain zero", map[string]string{"WHERRY_DRAIN_GRACE": "0s"}, "WHERRY_DRAIN_GRACE"},
		{"dashboard scheme", dash("https://example.test"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard port", dash("example.test:443"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard path", dash("example.test/admin"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard upper case", dash("Example.test"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard no dot", dash("dashboard"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard trailing dot", dash("dash.apps.example.test."), "WHERRY_DASHBOARD_HOST"},
		{"dashboard empty label", dash("example..test"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard ip", dash("192.0.2.1"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard numeric last label", dash("example.123"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard leading hyphen", dash("-a.example.com"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard hyphen last label", dash("foo.-"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard under domain", dash("dash.apps.example.test"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard deep under domain", dash("a.b.apps.example.test"), "WHERRY_DASHBOARD_HOST"},
		{"dashboard without https", map[string]string{"WHERRY_DOMAIN": "apps.example.test", "WHERRY_DASHBOARD_HOST": "example.test"}, "WHERRY_DASHBOARD_HOST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(func(k string) string { return tc.env[k] })
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("err = %q, want it to name %s", err, tc.wantVar)
			}
		})
	}
}

// Load reads the real process environment; t.Setenv restores the variable
// after the test and forbids t.Parallel for that reason.
func TestLoadReadsProcessEnv(t *testing.T) {
	t.Setenv("WHERRY_DOMAIN", "example.test")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Domain != "example.test" {
		t.Errorf("Domain = %q, want %q", got.Domain, "example.test")
	}
}

// Only a hand-built Config can have empty strings where load would have
// substituted a default, so validate is called directly here.
func TestValidateHandBuilt(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantVar string
	}{
		{"empty data dir", func(c *Config) { c.DataDir = "" }, "WHERRY_DATA_DIR"},
		{"empty domain", func(c *Config) { c.Domain = "" }, "WHERRY_DOMAIN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := c.validate()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("err = %q, want it to name %s", err, tc.wantVar)
			}
		})
	}
}

func TestLogger(t *testing.T) {
	cases := []struct {
		name   string
		format string
		level  slog.Level
		debug  bool   // log at debug instead of info
		want   string // substring of the output; empty means nothing is written
	}{
		{"json by default", LogFormatJSON, slog.LevelInfo, false, `"msg":"hello"`},
		{"text on request", LogFormatText, slog.LevelInfo, false, "msg=hello"},
		{"debug hidden at info", LogFormatJSON, slog.LevelInfo, true, ""},
		{"debug shown at debug", LogFormatJSON, slog.LevelDebug, true, `"msg":"hello"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.LogFormat, cfg.LogLevel = tc.format, tc.level
			var buf bytes.Buffer
			l := cfg.Logger(&buf)
			if tc.debug {
				l.Debug("hello", "k", "v")
			} else {
				l.Info("hello", "k", "v")
			}
			got := buf.String()
			if tc.want == "" {
				if got != "" {
					t.Fatalf("expected no output, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("output %q does not contain %q", got, tc.want)
			}
			if tc.format == LogFormatJSON && !json.Valid(buf.Bytes()) {
				t.Errorf("output is not valid JSON: %q", got)
			}
		})
	}
}
