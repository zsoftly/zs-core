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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var keycloakAuditSourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

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
	EnvKeycloakURL             = "ZS_KEYCLOAK_URL"
	EnvKeycloakRealm           = "ZS_KEYCLOAK_REALM"
	EnvKeycloakAdminUser       = "ZS_KEYCLOAK_ADMIN_USER"
	EnvKeycloakAdminPassword   = "ZS_KEYCLOAK_ADMIN_PASSWORD"
	EnvKeycloakAdminClientID   = "ZS_KEYCLOAK_ADMIN_CLIENT_ID"
	EnvKeycloakAdminSecret     = "ZS_KEYCLOAK_ADMIN_CLIENT_SECRET"
	EnvBrandsDir               = "ZS_BRANDS_DIR"
	EnvSecretsDir              = "ZS_SECRETS_DIR"
	EnvKillBillURL             = "ZS_KILLBILL_URL"
	EnvKillBillAdminUser       = "ZS_KILLBILL_ADMIN_USER"
	EnvKillBillAdminPassword   = "ZS_KILLBILL_ADMIN_PASSWORD"
	EnvKAUIURL                 = "ZS_KAUI_URL"
	EnvTenantSweepInterval     = "ZS_TENANT_SWEEP_INTERVAL"
	EnvKillBillWebhookSecret   = "ZS_KILLBILL_WEBHOOK_SECRET"
	EnvKillBillCallbackURL     = "ZS_KILLBILL_CALLBACK_URL"
	EnvCreditSweepInterval     = "ZS_CREDIT_SWEEP_INTERVAL"
	EnvWalletSweepInterval     = "ZS_WALLET_SWEEP_INTERVAL"
	EnvMeteringForwardInterval = "ZS_METERING_FORWARD_INTERVAL"
	EnvWalletCreditCacheTTL    = "ZS_WALLET_CREDIT_CACHE_TTL"
	EnvQuotaSweepInterval      = "ZS_QUOTA_SWEEP_INTERVAL"
	EnvUsagePartitionInterval  = "ZS_USAGE_PARTITION_INTERVAL"
	EnvKillBillDatabaseURL     = "ZS_KILLBILL_DATABASE_URL"
	EnvPDFRendererURL          = "ZS_PDF_RENDERER_URL"
	EnvPluginMirrorDir         = "ZS_PLUGIN_MIRROR_DIR"
	EnvPluginMirrorSecret      = "ZS_PLUGIN_MIRROR_SECRET"
	EnvPluginMirrorURL         = "ZS_PLUGIN_MIRROR_URL"
	// EnvKeycloakAuditSecrets maps an applied Keycloak realm name to its
	// deployment-injected HMAC key as realm=secret pairs separated by semicolons.
	// It is never read from a realm attribute or brand manifest.
	EnvKeycloakAuditSecrets    = "ZS_KEYCLOAK_AUDIT_SECRETS"
	EnvKeycloakAuditSourceID   = "ZS_KEYCLOAK_AUDIT_SOURCE_ID"
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
	DefaultTenantSweepInterval   = time.Minute
	DefaultCreditSweepInterval   = time.Hour
	DefaultWalletSweepInterval   = time.Minute
	DefaultMeteringForward       = time.Minute
	DefaultWalletCreditCacheTTL  = 5 * time.Second
	DefaultQuotaSweepInterval    = time.Minute
	DefaultUsagePartition        = time.Hour
	// maxWalletCreditCacheTTL caps ZS_WALLET_CREDIT_CACHE_TTL. A longer
	// cache would let reservations rely on a Kill Bill credit long out of
	// date.
	maxWalletCreditCacheTTL = time.Minute
	maxDatabaseMaxConns     = 1000
	// minWebhookSecretLength is the shortest ZS_KILLBILL_WEBHOOK_SECRET
	// and ZS_PLUGIN_MIRROR_SECRET.
	minWebhookSecretLength = 32
)

var known = map[string]bool{
	EnvDatabaseURL: true, EnvDatabaseMaxConns: true, EnvListenAddr: true, EnvLogLevel: true,
	EnvLogFormat: true, EnvMigrateOnStart: true, EnvHTTPReadHeaderTimeout: true,
	EnvHTTPReadTimeout: true, EnvHTTPWriteTimeout: true, EnvHTTPIdleTimeout: true,
	EnvShutdownTimeout: true,
	EnvKeycloakURL:     true, EnvKeycloakRealm: true, EnvKeycloakAdminUser: true,
	EnvKeycloakAdminPassword: true, EnvKillBillURL: true, EnvKillBillAdminUser: true,
	EnvKillBillAdminPassword: true, EnvKAUIURL: true,
	EnvKeycloakAdminClientID: true, EnvKeycloakAdminSecret: true, EnvBrandsDir: true, EnvSecretsDir: true,
	EnvTenantSweepInterval: true, EnvKillBillWebhookSecret: true, EnvKillBillCallbackURL: true, EnvCreditSweepInterval: true,
	EnvWalletSweepInterval: true, EnvMeteringForwardInterval: true, EnvWalletCreditCacheTTL: true,
	EnvQuotaSweepInterval: true, EnvUsagePartitionInterval: true,
	EnvKillBillDatabaseURL: true, EnvPDFRendererURL: true, EnvPluginMirrorDir: true,
	EnvPluginMirrorSecret: true, EnvPluginMirrorURL: true,
	EnvKeycloakAuditSecrets:  true,
	EnvKeycloakAuditSourceID: true,
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

	// The component settings below are read by `zs-core doctor`. All of them
	// are optional for `serve` and `migrate`. An admin user and its password
	// are set together or not at all.
	KeycloakURL           string
	KeycloakRealm         string
	KeycloakAdminUser     string
	KeycloakAdminPassword string
	// KeycloakAdminClientID and KeycloakAdminClientSecret select the client
	// credentials grant for the Admin API instead of the admin password.
	KeycloakAdminClientID     string
	KeycloakAdminClientSecret string
	KillBillURL               string
	KillBillAdminUser         string
	KillBillAdminPassword     string
	KAUIURL                   string

	// BrandsDir holds brand manifests, brands/<slug>.yaml. serve loads and
	// validates them at start and resolves the file references of HTTP
	// applies against it. Apply stays an explicit command.
	BrandsDir string

	// SecretsDir holds file secrets of brand manifests, one directory per
	// brand slug. A file:// reference of brand <slug> must resolve inside
	// SecretsDir/<slug>/. When empty, file:// references are refused.
	SecretsDir string

	// TenantSweepInterval is how often serve completes interrupted tenant
	// creations and corrects Keycloak Organizations towards Core.
	TenantSweepInterval time.Duration

	// KillBillWebhookSecret verifies Kill Bill push notifications, through a
	// token per brand in the callback path, and signs billing credentials.
	// At least 32 characters.
	KillBillWebhookSecret string

	// KillBillCallbackURL is Core's base URL as Kill Bill reaches it, such
	// as http://core:8080. With the webhook secret, brand apply registers
	// each brand's push notification callback under it.
	KillBillCallbackURL string

	// CreditSweepInterval is how often serve expires credits Core issued.
	CreditSweepInterval time.Duration

	// WalletSweepInterval is how often serve expires reservations past
	// their lease, drives the settlements that wait for Kill Bill and
	// reconciles the wallet view with Kill Bill.
	WalletSweepInterval time.Duration

	// QuotaSweepInterval is how often serve releases concurrent quota
	// leases past their expiry.
	QuotaSweepInterval time.Duration

	// UsagePartitionInterval is how often serve makes sure the usage event
	// partitions of the current month and the next three exist.
	UsagePartitionInterval time.Duration

	// MeteringForwardInterval is how often serve forwards recorded usage
	// to Kill Bill, besides the forward each recording triggers.
	MeteringForwardInterval time.Duration

	// WalletCreditCacheTTL is how long a reservation may rely on the Kill
	// Bill account credit it last read. At most one minute.
	WalletCreditCacheTTL time.Duration

	// KillBillDatabaseURL is the Kill Bill database, for deployment steps
	// only: zs-core plugin ddl apply creates plugin tables there, doctor
	// checks them, and brand apply on the command line refuses a plugin
	// whose tables are missing. serve never uses it.
	KillBillDatabaseURL string

	// PDFRendererURL is the PDF renderer, such as http://gotenberg:3000.
	// Without it invoice PDFs answer 404 invoice_pdf_unavailable.
	PDFRendererURL string

	// PluginMirrorDir holds the pinned plugin jars, synced by zs-core
	// plugin mirror sync. With PluginMirrorSecret, serve serves them at
	// GET /v1/plugins/{name}/{version}.jar.
	PluginMirrorDir string

	// PluginMirrorSecret protects the mirror route. At least 32
	// characters.
	PluginMirrorSecret string

	// PluginMirrorURL is Core's base URL as Kill Bill reaches the mirror.
	// New plugin installs require it and PluginMirrorSecret; KPM downloads
	// only from the verified mirror. Starting an already-installed plugin
	// does not download a jar.
	PluginMirrorURL string
	// KeycloakAuditSecrets authorizes the Keycloak audit listener by realm.
	KeycloakAuditSecrets  map[string][]byte
	KeycloakAuditSourceID string
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
		DatabaseMaxConns:        DefaultDatabaseMaxConns,
		ListenAddr:              DefaultListenAddr,
		LogFormat:               DefaultLogFormat,
		MigrateOnStart:          DefaultMigrateOnStart,
		HTTPReadHeaderTimeout:   DefaultHTTPReadHeaderTimeout,
		HTTPReadTimeout:         DefaultHTTPReadTimeout,
		HTTPWriteTimeout:        DefaultHTTPWriteTimeout,
		HTTPIdleTimeout:         DefaultHTTPIdleTimeout,
		ShutdownTimeout:         DefaultShutdownTimeout,
		TenantSweepInterval:     DefaultTenantSweepInterval,
		CreditSweepInterval:     DefaultCreditSweepInterval,
		WalletSweepInterval:     DefaultWalletSweepInterval,
		MeteringForwardInterval: DefaultMeteringForward,
		WalletCreditCacheTTL:    DefaultWalletCreditCacheTTL,
		QuotaSweepInterval:      DefaultQuotaSweepInterval,
		UsagePartitionInterval:  DefaultUsagePartition,
		KeycloakAuditSourceID:   "keycloak",
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
	parseDuration(EnvTenantSweepInterval, &cfg.TenantSweepInterval)
	parseDuration(EnvCreditSweepInterval, &cfg.CreditSweepInterval)
	parseDuration(EnvWalletSweepInterval, &cfg.WalletSweepInterval)
	parseDuration(EnvMeteringForwardInterval, &cfg.MeteringForwardInterval)
	parseDuration(EnvWalletCreditCacheTTL, &cfg.WalletCreditCacheTTL)
	parseDuration(EnvQuotaSweepInterval, &cfg.QuotaSweepInterval)
	parseDuration(EnvUsagePartitionInterval, &cfg.UsagePartitionInterval)
	if cfg.WalletCreditCacheTTL > maxWalletCreditCacheTTL {
		fail("%s must be at most %s", EnvWalletCreditCacheTTL, maxWalletCreditCacheTTL)
	}

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
	parseURL(EnvKillBillCallbackURL, &cfg.KillBillCallbackURL)
	parseURL(EnvPDFRendererURL, &cfg.PDFRendererURL)
	parseURL(EnvPluginMirrorURL, &cfg.PluginMirrorURL)
	parseSecret := func(name string, target *string) {
		if v, ok := env[name]; ok && v != "" {
			if len(v) < minWebhookSecretLength {
				fail("%s must be at least %d characters", name, minWebhookSecretLength)
			} else {
				*target = v
			}
		}
	}
	parseSecret(EnvKillBillWebhookSecret, &cfg.KillBillWebhookSecret)
	parseSecret(EnvPluginMirrorSecret, &cfg.PluginMirrorSecret)
	if raw := env[EnvKeycloakAuditSecrets]; raw != "" {
		cfg.KeycloakAuditSecrets = map[string][]byte{}
		for _, pair := range strings.Split(raw, ";") {
			realm, secret, ok := strings.Cut(pair, "=")
			if !ok || realm == "" || strings.ContainsAny(realm, "=;\x00") || len(secret) < minWebhookSecretLength || cfg.KeycloakAuditSecrets[realm] != nil {
				fail("%s must be unique realm=secret pairs with secrets at least %d characters", EnvKeycloakAuditSecrets, minWebhookSecretLength)
				break
			}
			cfg.KeycloakAuditSecrets[realm] = []byte(secret)
		}
	}
	if raw := env[EnvKeycloakAuditSourceID]; raw != "" {
		if !keycloakAuditSourceID.MatchString(raw) {
			fail("%s must contain only letters, digits, underscores and hyphens", EnvKeycloakAuditSourceID)
		} else {
			cfg.KeycloakAuditSourceID = raw
		}
	}
	if v := env[EnvKillBillDatabaseURL]; strings.TrimSpace(v) != "" {
		if err := validateDatabaseURL(v); err != nil {
			fail("%s %s", EnvKillBillDatabaseURL, err)
		} else {
			cfg.KillBillDatabaseURL = v
		}
	}
	if v := env[EnvPluginMirrorDir]; v != "" {
		if strings.ContainsRune(v, 0) || !filepath.IsAbs(v) {
			fail("%s must be an absolute directory path", EnvPluginMirrorDir)
		} else {
			cfg.PluginMirrorDir = filepath.Clean(v)
		}
	}
	if env[EnvPluginMirrorURL] != "" && env[EnvPluginMirrorSecret] == "" {
		fail("%s needs %s", EnvPluginMirrorURL, EnvPluginMirrorSecret)
	}
	cfg.KeycloakRealm = env[EnvKeycloakRealm]
	cfg.KeycloakAdminUser = env[EnvKeycloakAdminUser]
	cfg.KeycloakAdminPassword = env[EnvKeycloakAdminPassword]
	cfg.KillBillAdminUser = env[EnvKillBillAdminUser]
	cfg.KillBillAdminPassword = env[EnvKillBillAdminPassword]
	if (cfg.KeycloakAdminUser == "") != (cfg.KeycloakAdminPassword == "") {
		fail("%s and %s must be set together", EnvKeycloakAdminUser, EnvKeycloakAdminPassword)
	}
	cfg.KeycloakAdminClientID = env[EnvKeycloakAdminClientID]
	cfg.KeycloakAdminClientSecret = env[EnvKeycloakAdminSecret]
	if (cfg.KeycloakAdminClientID == "") != (cfg.KeycloakAdminClientSecret == "") {
		fail("%s and %s must be set together", EnvKeycloakAdminClientID, EnvKeycloakAdminSecret)
	}
	if v := env[EnvBrandsDir]; v != "" {
		if strings.ContainsRune(v, 0) {
			fail("%s must be a directory path", EnvBrandsDir)
		} else {
			cfg.BrandsDir = v
		}
	}
	if v := env[EnvSecretsDir]; v != "" {
		if strings.ContainsRune(v, 0) || !filepath.IsAbs(v) {
			fail("%s must be an absolute directory path", EnvSecretsDir)
		} else {
			cfg.SecretsDir = filepath.Clean(v)
		}
	}
	if (cfg.KillBillAdminUser == "") != (cfg.KillBillAdminPassword == "") {
		fail("%s and %s must be set together", EnvKillBillAdminUser, EnvKillBillAdminPassword)
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
