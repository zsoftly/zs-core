// Package config loads the service configuration from the environment.
//
// Configuration is environment only. Every variable is listed in
// docs/configuration.md. Load validates everything at start and reports every
// problem at once. A variable that starts with ZS_ but is not known is an
// error, so a typo cannot silently fall back to a default. Values are never
// echoed in errors, because some of them hold credentials.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Variable names.
const (
	EnvDatabaseURL             = "ZS_DATABASE_URL"
	EnvDatabaseMaxConns        = "ZS_DATABASE_MAX_CONNS"
	EnvListenAddr              = "ZS_LISTEN_ADDR"
	EnvLogLevel                = "ZS_LOG_LEVEL"
	EnvLogFormat               = "ZS_LOG_FORMAT"
	EnvMigrateOnStart          = "ZS_MIGRATE_ON_START"
	EnvHTTPReadHeaderTimeout   = "ZS_HTTP_READ_HEADER_TIMEOUT"
	EnvHTTPReadTimeout         = "ZS_HTTP_READ_TIMEOUT"
	EnvHTTPWriteTimeout        = "ZS_HTTP_WRITE_TIMEOUT"
	EnvHTTPIdleTimeout         = "ZS_HTTP_IDLE_TIMEOUT"
	EnvShutdownTimeout         = "ZS_SHUTDOWN_TIMEOUT"
	EnvDevTrustedScopeHeaders  = "ZS_DEV_TRUSTED_SCOPE_HEADERS"
	EnvKeycloakURL             = "ZS_KEYCLOAK_URL"
	EnvKeycloakRealm           = "ZS_KEYCLOAK_REALM"
	EnvKeycloakAdminUser       = "ZS_KEYCLOAK_ADMIN_USER"
	EnvKeycloakAdminPassword   = "ZS_KEYCLOAK_ADMIN_PASSWORD"
	EnvKillBillURL             = "ZS_KILLBILL_URL"
	EnvKillBillAdminUser       = "ZS_KILLBILL_ADMIN_USER"
	EnvKillBillAdminPassword   = "ZS_KILLBILL_ADMIN_PASSWORD"
	EnvKAUIURL                 = "ZS_KAUI_URL"
	reservedTestVariablePrefix = "ZS_TEST_"
)

// Defaults.
const (
	DefaultListenAddr            = "127.0.0.1:8080"
	DefaultDatabaseMaxConns      = 10
	DefaultLogLevel              = "info"
	DefaultLogFormat             = "json"
	DefaultMigrateOnStart        = true
	DefaultHTTPReadHeaderTimeout = 5 * time.Second
	DefaultHTTPReadTimeout       = 30 * time.Second
	DefaultHTTPWriteTimeout      = 30 * time.Second
	DefaultHTTPIdleTimeout       = 120 * time.Second
	DefaultShutdownTimeout       = 15 * time.Second
	maxDatabaseMaxConns          = 1000
)

var known = map[string]bool{
	EnvDatabaseURL: true, EnvDatabaseMaxConns: true, EnvListenAddr: true, EnvLogLevel: true,
	EnvLogFormat: true, EnvMigrateOnStart: true, EnvHTTPReadHeaderTimeout: true,
	EnvHTTPReadTimeout: true, EnvHTTPWriteTimeout: true, EnvHTTPIdleTimeout: true,
	EnvShutdownTimeout: true, EnvDevTrustedScopeHeaders: true,
	EnvKeycloakURL: true, EnvKeycloakRealm: true, EnvKeycloakAdminUser: true,
	EnvKeycloakAdminPassword: true, EnvKillBillURL: true, EnvKillBillAdminUser: true,
	EnvKillBillAdminPassword: true, EnvKAUIURL: true,
}

// Config is the validated service configuration.
type Config struct {
	DatabaseURL           string
	DatabaseMaxConns      int32
	ListenAddr            string
	LogLevel              slog.Level
	LogFormat             string
	MigrateOnStart        bool
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration
	ShutdownTimeout       time.Duration
	// DevTrustedScopeHeaders makes the API read the brand and tenant from
	// the X-ZS-Brand and X-ZS-Tenant headers without authentication. It is a
	// development aid for Phase 1 and is refused unless ListenAddr is a
	// loopback address.
	DevTrustedScopeHeaders bool

	// The component settings below are read by `zs-core doctor`. All of them
	// are optional for `serve` and `migrate`. An admin user and its password
	// are set together or not at all.
	KeycloakURL           string
	KeycloakRealm         string
	KeycloakAdminUser     string
	KeycloakAdminPassword string
	KillBillURL           string
	KillBillAdminUser     string
	KillBillAdminPassword string
	KAUIURL               string
}

// Load reads the configuration from environ, a list of KEY=value strings as
// returned by os.Environ.
func Load(environ []string) (Config, error) {
	env := map[string]string{}
	var problems []string
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "ZS_") {
			continue
		}
		if !known[key] && !strings.HasPrefix(key, reservedTestVariablePrefix) {
			problems = append(problems, fmt.Sprintf("%s is not a known variable", key))
			continue
		}
		env[key] = value
	}

	cfg := Config{
		DatabaseMaxConns:      DefaultDatabaseMaxConns,
		ListenAddr:            DefaultListenAddr,
		LogFormat:             DefaultLogFormat,
		MigrateOnStart:        DefaultMigrateOnStart,
		HTTPReadHeaderTimeout: DefaultHTTPReadHeaderTimeout,
		HTTPReadTimeout:       DefaultHTTPReadTimeout,
		HTTPWriteTimeout:      DefaultHTTPWriteTimeout,
		HTTPIdleTimeout:       DefaultHTTPIdleTimeout,
		ShutdownTimeout:       DefaultShutdownTimeout,
	}
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if v, ok := env[EnvDatabaseURL]; !ok || strings.TrimSpace(v) == "" {
		fail("%s is required", EnvDatabaseURL)
	} else if err := validateDatabaseURL(v); err != nil {
		fail("%s %s", EnvDatabaseURL, err)
	} else {
		cfg.DatabaseURL = v
	}

	if v, ok := env[EnvDatabaseMaxConns]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxDatabaseMaxConns {
			fail("%s must be an integer from 1 to %d", EnvDatabaseMaxConns, maxDatabaseMaxConns)
		} else {
			cfg.DatabaseMaxConns = int32(n)
		}
	}

	if v, ok := env[EnvListenAddr]; ok {
		cfg.ListenAddr = v
	}
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err != nil || port == "" {
		fail("%s must be host:port", EnvListenAddr)
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		fail("%s port must be 0 to 65535", EnvListenAddr)
	}

	level := DefaultLogLevel
	if v, ok := env[EnvLogLevel]; ok {
		level = v
	}
	switch level {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		fail("%s must be debug, info, warn or error", EnvLogLevel)
	}

	if v, ok := env[EnvLogFormat]; ok {
		if v != "json" && v != "text" {
			fail("%s must be json or text", EnvLogFormat)
		} else {
			cfg.LogFormat = v
		}
	}

	parseBool := func(name string, target *bool) {
		v, ok := env[name]
		if !ok {
			return
		}
		switch v {
		case "true":
			*target = true
		case "false":
			*target = false
		default:
			fail("%s must be true or false", name)
		}
	}
	parseBool(EnvMigrateOnStart, &cfg.MigrateOnStart)
	parseBool(EnvDevTrustedScopeHeaders, &cfg.DevTrustedScopeHeaders)

	parseDuration := func(name string, target *time.Duration) {
		v, ok := env[name]
		if !ok {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail("%s must be a positive Go duration such as 30s", name)
			return
		}
		*target = d
	}
	parseDuration(EnvHTTPReadHeaderTimeout, &cfg.HTTPReadHeaderTimeout)
	parseDuration(EnvHTTPReadTimeout, &cfg.HTTPReadTimeout)
	parseDuration(EnvHTTPWriteTimeout, &cfg.HTTPWriteTimeout)
	parseDuration(EnvHTTPIdleTimeout, &cfg.HTTPIdleTimeout)
	parseDuration(EnvShutdownTimeout, &cfg.ShutdownTimeout)

	parseURL := func(name string, target *string) {
		v, ok := env[name]
		if !ok || v == "" {
			return
		}
		if err := validateHTTPURL(v); err != nil {
			fail("%s %s", name, err)
			return
		}
		*target = strings.TrimRight(v, "/")
	}
	parseURL(EnvKeycloakURL, &cfg.KeycloakURL)
	parseURL(EnvKillBillURL, &cfg.KillBillURL)
	parseURL(EnvKAUIURL, &cfg.KAUIURL)
	cfg.KeycloakRealm = env[EnvKeycloakRealm]
	cfg.KeycloakAdminUser = env[EnvKeycloakAdminUser]
	cfg.KeycloakAdminPassword = env[EnvKeycloakAdminPassword]
	cfg.KillBillAdminUser = env[EnvKillBillAdminUser]
	cfg.KillBillAdminPassword = env[EnvKillBillAdminPassword]
	if (cfg.KeycloakAdminUser == "") != (cfg.KeycloakAdminPassword == "") {
		fail("%s and %s must be set together", EnvKeycloakAdminUser, EnvKeycloakAdminPassword)
	}
	if (cfg.KillBillAdminUser == "") != (cfg.KillBillAdminPassword == "") {
		fail("%s and %s must be set together", EnvKillBillAdminUser, EnvKillBillAdminPassword)
	}

	if cfg.DevTrustedScopeHeaders && !IsLoopback(cfg.ListenAddr) {
		fail("%s=true requires %s to be a loopback address such as 127.0.0.1:8080", EnvDevTrustedScopeHeaders, EnvListenAddr)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("invalid configuration: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a postgres:// or postgresql:// URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("must be a postgres:// or postgresql:// URL")
	}
	if u.Host == "" && u.Query().Get("host") == "" {
		return errors.New("must name a host")
	}
	return nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http:// or https:// URL with a host")
	}
	if u.User != nil {
		return errors.New("must not contain a user name or password; use the *_ADMIN_USER and *_ADMIN_PASSWORD settings")
	}
	return nil
}

// IsLoopback reports whether the host part of addr is localhost or a
// loopback IP. An empty host, which listens on every interface, is not.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
