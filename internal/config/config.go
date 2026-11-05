// Package config reads the daemon's settings from WHERRY_* environment
// variables, with defaults for everything and validation that names the
// variable at fault.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// hostKeyFile is the SSH host key's file name inside DataDir.
const hostKeyFile = "host_ed25519"

// Accepted values for Config.Runtime and Config.LogFormat.
const (
	RuntimeDocker = "docker"
	RuntimeNative = "native"
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// hostnameRE is a bare lower-case hostname with at least two labels: no
// scheme, port, or path, no empty label, trailing dot included, and no label
// that starts or ends with a hyphen. The last label starts with a letter,
// which also rules out an IP address: no certificate can be had for one
// this way.
var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Config holds every WHERRY_* setting. Loaded once at startup, read-only after.
type Config struct {
	DataDir          string
	Domain           string
	HTTPAddr         string
	HTTPSAddr        string // empty disables TLS
	ACMEEmail        string
	DashboardHost    string // public hostname of the dashboard; empty keeps it internal
	SSHAddr          string
	SSHHostKey       string
	InternalAddr     string
	Runtime          string // RuntimeDocker or RuntimeNative
	CgroupRoot       string // where the native runtime makes instance cgroups
	DockerHost       string
	BuildConcurrency int
	BuildTimeout     time.Duration
	HealthTimeout    time.Duration
	DrainGrace       time.Duration
	StopGrace        time.Duration
	RateLimitRPS     int // 0 disables
	LogLevel         slog.Level
	LogFormat        string // LogFormatJSON or LogFormatText
}

// Default returns the settings used when no WHERRY_* variable is set.
func Default() Config {
	return Config{
		DataDir:          "./data",
		Domain:           "localhost",
		HTTPAddr:         ":8080",
		SSHAddr:          ":2222",
		SSHHostKey:       filepath.Join("./data", hostKeyFile),
		InternalAddr:     "127.0.0.1:9000",
		Runtime:          RuntimeDocker,
		CgroupRoot:       "/sys/fs/cgroup/wherry",
		BuildConcurrency: 2,
		BuildTimeout:     10 * time.Minute,
		HealthTimeout:    30 * time.Second,
		DrainGrace:       5 * time.Second,
		StopGrace:        10 * time.Second,
		RateLimitRPS:     100,
		LogLevel:         slog.LevelInfo,
		LogFormat:        LogFormatJSON,
	}
}

// Logger builds the daemon's logger as configured: JSON for machines, text
// for a terminal, at LogLevel. Config values themselves are never logged.
func (c Config) Logger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Load reads WHERRY_* from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

// load is Load with the environment lookup injected so tests never touch the
// process environment. An empty value is treated as unset.
func load(getenv func(string) string) (Config, error) {
	c := Default()
	if v := getenv("WHERRY_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := getenv("WHERRY_DOMAIN"); v != "" {
		c.Domain = v
	}
	if v := getenv("WHERRY_HTTP_ADDR"); v != "" {
		c.HTTPAddr = v
	}
	if v := getenv("WHERRY_HTTPS_ADDR"); v != "" {
		c.HTTPSAddr = v
	}
	if v := getenv("WHERRY_ACME_EMAIL"); v != "" {
		c.ACMEEmail = v
	}
	if v := getenv("WHERRY_DASHBOARD_HOST"); v != "" {
		c.DashboardHost = v
	}
	if v := getenv("WHERRY_SSH_ADDR"); v != "" {
		c.SSHAddr = v
	}
	// The host key follows DataDir unless set explicitly, so DataDir must be read first.
	if v := getenv("WHERRY_SSH_HOST_KEY"); v != "" {
		c.SSHHostKey = v
	} else {
		c.SSHHostKey = filepath.Join(c.DataDir, hostKeyFile)
	}
	if v := getenv("WHERRY_INTERNAL_ADDR"); v != "" {
		c.InternalAddr = v
	}
	if v := getenv("WHERRY_RUNTIME"); v != "" {
		c.Runtime = v
	}
	if v := getenv("WHERRY_CGROUP_ROOT"); v != "" {
		c.CgroupRoot = v
	}
	if v := getenv("WHERRY_DOCKER_HOST"); v != "" {
		c.DockerHost = v
	}
	if v := getenv("WHERRY_LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}

	var err error
	c.BuildConcurrency, err = lookupInt(getenv, "WHERRY_BUILD_CONCURRENCY", c.BuildConcurrency)
	if err != nil {
		return Config{}, err
	}
	c.RateLimitRPS, err = lookupInt(getenv, "WHERRY_RATE_LIMIT_RPS", c.RateLimitRPS)
	if err != nil {
		return Config{}, err
	}
	c.BuildTimeout, err = lookupDuration(getenv, "WHERRY_BUILD_TIMEOUT", c.BuildTimeout)
	if err != nil {
		return Config{}, err
	}
	c.HealthTimeout, err = lookupDuration(getenv, "WHERRY_HEALTH_TIMEOUT", c.HealthTimeout)
	if err != nil {
		return Config{}, err
	}
	c.DrainGrace, err = lookupDuration(getenv, "WHERRY_DRAIN_GRACE", c.DrainGrace)
	if err != nil {
		return Config{}, err
	}
	c.StopGrace, err = lookupDuration(getenv, "WHERRY_STOP_GRACE", c.StopGrace)
	if err != nil {
		return Config{}, err
	}
	if v := getenv("WHERRY_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("WHERRY_LOG_LEVEL: %w", err)
		}
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate checks ranges, enums, listener addresses, and the cross-field
// rules. Every error starts with the variable name so the operator can fix it
// without reading this file.
func (c Config) validate() error {
	if c.DataDir == "" {
		return errors.New("WHERRY_DATA_DIR: must not be empty")
	}
	if c.Domain == "" {
		return errors.New("WHERRY_DOMAIN: must not be empty")
	}
	if err := checkAddr("WHERRY_HTTP_ADDR", c.HTTPAddr); err != nil {
		return err
	}
	if err := checkAddr("WHERRY_SSH_ADDR", c.SSHAddr); err != nil {
		return err
	}
	if err := checkAddr("WHERRY_INTERNAL_ADDR", c.InternalAddr); err != nil {
		return err
	}
	if c.HTTPSAddr != "" {
		if err := checkAddr("WHERRY_HTTPS_ADDR", c.HTTPSAddr); err != nil {
			return err
		}
		if c.ACMEEmail == "" {
			return errors.New("WHERRY_ACME_EMAIL: required when WHERRY_HTTPS_ADDR is set")
		}
	}
	if err := c.checkDashboardHost(); err != nil {
		return err
	}
	switch c.Runtime {
	case RuntimeDocker, RuntimeNative:
	default:
		return fmt.Errorf("WHERRY_RUNTIME: %q is not %s or %s", c.Runtime, RuntimeDocker, RuntimeNative)
	}
	if !filepath.IsAbs(c.CgroupRoot) {
		return fmt.Errorf("WHERRY_CGROUP_ROOT: %q is not an absolute path", c.CgroupRoot)
	}
	if c.BuildConcurrency < 1 {
		return fmt.Errorf("WHERRY_BUILD_CONCURRENCY: %d is less than 1", c.BuildConcurrency)
	}
	if c.BuildTimeout <= 0 {
		return fmt.Errorf("WHERRY_BUILD_TIMEOUT: %v is not positive", c.BuildTimeout)
	}
	if c.HealthTimeout <= 0 {
		return fmt.Errorf("WHERRY_HEALTH_TIMEOUT: %v is not positive", c.HealthTimeout)
	}
	if c.DrainGrace <= 0 {
		return fmt.Errorf("WHERRY_DRAIN_GRACE: %v is not positive", c.DrainGrace)
	}
	if c.StopGrace <= 0 {
		return fmt.Errorf("WHERRY_STOP_GRACE: %v is not positive", c.StopGrace)
	}
	if c.RateLimitRPS < 0 {
		return fmt.Errorf("WHERRY_RATE_LIMIT_RPS: %d is negative", c.RateLimitRPS)
	}
	switch c.LogFormat {
	case LogFormatJSON, LogFormatText:
	default:
		return fmt.Errorf("WHERRY_LOG_FORMAT: %q is not %s or %s", c.LogFormat, LogFormatJSON, LogFormatText)
	}
	return nil
}

// checkDashboardHost holds the rules for the public dashboard. The admin
// token is typed into that page, so it must never cross plain HTTP. A name
// under the domain belongs to the app of that name; the domain itself is
// free, since an app host is always <name>.<Domain>.
func (c Config) checkDashboardHost() error {
	if c.DashboardHost == "" {
		return nil
	}
	if !hostnameRE.MatchString(c.DashboardHost) {
		return fmt.Errorf("WHERRY_DASHBOARD_HOST: %q is not a bare lower-case hostname", c.DashboardHost)
	}
	if c.HTTPSAddr == "" {
		return errors.New("WHERRY_DASHBOARD_HOST: requires WHERRY_HTTPS_ADDR")
	}
	domain := strings.TrimSuffix(strings.ToLower(c.Domain), ".")
	if strings.HasSuffix(c.DashboardHost, "."+domain) {
		return fmt.Errorf("WHERRY_DASHBOARD_HOST: %q is under WHERRY_DOMAIN, so it belongs to an app", c.DashboardHost)
	}
	return nil
}

// checkAddr accepts anything net.Listen would take as a TCP address, such as
// ":8080", "127.0.0.1:9000", or "[::1]:80". SplitHostPort allows an empty port,
// so that is rejected here.
func checkAddr(key, val string) error {
	_, port, err := net.SplitHostPort(val)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if port == "" {
		return fmt.Errorf("%s: %q has no port", key, val)
	}
	return nil
}

// lookupInt returns def when key is unset, otherwise the parsed value.
// Parse errors are prefixed with the variable name.
func lookupInt(getenv func(string) string, key string, def int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func lookupDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
