package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

const dbURL = "ZS_DATABASE_URL=postgres://zs:secret-password@db.internal:5432/zs?sslmode=require"

func TestDefaults(t *testing.T) {
	cfg, err := Load([]string{dbURL, "PATH=/usr/bin", "HOME=/home/x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != DefaultListenAddr || cfg.DatabaseMaxConns != DefaultDatabaseMaxConns ||
		cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != "json" || !cfg.MigrateOnStart ||
		cfg.BrandsDir != "" || cfg.SecretsDir != "" || cfg.ShutdownTimeout != DefaultShutdownTimeout ||
		cfg.HTTPReadHeaderTimeout != DefaultHTTPReadHeaderTimeout {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := Load([]string{
		dbURL,
		"ZS_DATABASE_MAX_CONNS=25",
		"ZS_LISTEN_ADDR=[::1]:9090",
		"ZS_LOG_LEVEL=debug",
		"ZS_LOG_FORMAT=text",
		"ZS_MIGRATE_ON_START=false",
		"ZS_HTTP_READ_HEADER_TIMEOUT=2s",
		"ZS_HTTP_READ_TIMEOUT=10s",
		"ZS_HTTP_WRITE_TIMEOUT=11s",
		"ZS_HTTP_IDLE_TIMEOUT=1m",
		"ZS_SHUTDOWN_TIMEOUT=3s",
		"ZS_TENANT_SWEEP_INTERVAL=2m",
		"ZS_BRANDS_DIR=/etc/zs-core/brands",
		"ZS_SECRETS_DIR=/run/zs-core/secrets/",
		"ZS_TEST_POSTGRES_URL=postgres://ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseMaxConns != 25 || cfg.ListenAddr != "[::1]:9090" || cfg.LogLevel != slog.LevelDebug ||
		cfg.LogFormat != "text" || cfg.MigrateOnStart || cfg.BrandsDir != "/etc/zs-core/brands" ||
		cfg.SecretsDir != "/run/zs-core/secrets" ||
		cfg.HTTPReadHeaderTimeout != 2*time.Second || cfg.HTTPReadTimeout != 10*time.Second ||
		cfg.HTTPWriteTimeout != 11*time.Second || cfg.HTTPIdleTimeout != time.Minute || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.TenantSweepInterval != 2*time.Minute {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestStrictValidationReportsEveryProblemWithoutValues(t *testing.T) {
	_, err := Load([]string{
		"ZS_DATABASE_URL=mysql://root:hunter2@db/x",
		"ZS_DATABASE_MAX_CONNS=0",
		"ZS_LISTEN_ADDR=nope",
		"ZS_LOG_LEVEL=loud",
		"ZS_LOG_FORMAT=xml",
		"ZS_MIGRATE_ON_START=yes",
		"ZS_SHUTDOWN_TIMEOUT=-1s",
		"ZS_LISTEN_ADRR=127.0.0.1:1",
		"ZS_SECRETS_DIR=relative/secrets",
	})
	if err == nil {
		t.Fatal("Load succeeded")
	}
	msg := err.Error()
	for _, want := range []string{
		"ZS_DATABASE_URL", "ZS_DATABASE_MAX_CONNS", "ZS_LISTEN_ADDR must be host:port", "ZS_LOG_LEVEL",
		"ZS_LOG_FORMAT", "ZS_MIGRATE_ON_START", "ZS_SHUTDOWN_TIMEOUT", "ZS_LISTEN_ADRR is not a known variable",
		"ZS_SECRETS_DIR must be an absolute directory path",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "hunter2") {
		t.Fatal("error echoed a credential")
	}
}

func TestDatabaseURLRequired(t *testing.T) {
	if _, err := Load(nil); err == nil || !strings.Contains(err.Error(), "ZS_DATABASE_URL is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestTrustedScopeHeadersAreGone(t *testing.T) {
	_, err := Load([]string{dbURL, "ZS_DEV_TRUSTED_SCOPE_HEADERS=true", "ZS_LISTEN_ADDR=127.0.0.1:8080"})
	if err == nil || !strings.Contains(err.Error(), "ZS_DEV_TRUSTED_SCOPE_HEADERS is not a known variable") {
		t.Fatalf("the Phase 1 trusted-header flag must be refused: %v", err)
	}
}

func TestKeycloakAdminClientCredentials(t *testing.T) {
	cfg, err := Load([]string{dbURL, "ZS_KEYCLOAK_ADMIN_CLIENT_ID=zs-core-admin", "ZS_KEYCLOAK_ADMIN_CLIENT_SECRET=pw-client"})
	if err != nil || cfg.KeycloakAdminClientID != "zs-core-admin" || cfg.KeycloakAdminClientSecret != "pw-client" {
		t.Fatalf("client credentials: %+v %v", cfg, err)
	}
	_, err = Load([]string{dbURL, "ZS_KEYCLOAK_ADMIN_CLIENT_SECRET=pw-client"})
	if err == nil || !strings.Contains(err.Error(), "ZS_KEYCLOAK_ADMIN_CLIENT_ID and ZS_KEYCLOAK_ADMIN_CLIENT_SECRET must be set together") ||
		strings.Contains(err.Error(), "pw-client") {
		t.Fatalf("half-set client credentials: %v", err)
	}
}

func TestComponentSettings(t *testing.T) {
	cfg, err := Load([]string{
		dbURL,
		"ZS_KEYCLOAK_URL=http://keycloak:8080/",
		"ZS_KEYCLOAK_REALM=zs-reference",
		"ZS_KEYCLOAK_ADMIN_USER=admin",
		"ZS_KEYCLOAK_ADMIN_PASSWORD=kc-secret",
		"ZS_KILLBILL_URL=https://billing.internal",
		"ZS_KILLBILL_ADMIN_USER=admin",
		"ZS_KILLBILL_ADMIN_PASSWORD=kb-secret",
		"ZS_KAUI_URL=http://kaui:8080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeycloakURL != "http://keycloak:8080" || cfg.KeycloakRealm != "zs-reference" ||
		cfg.KeycloakAdminUser != "admin" || cfg.KeycloakAdminPassword != "kc-secret" ||
		cfg.KillBillURL != "https://billing.internal" || cfg.KillBillAdminUser != "admin" ||
		cfg.KillBillAdminPassword != "kb-secret" || cfg.KAUIURL != "http://kaui:8080" {
		t.Fatalf("component settings not applied: %+v", cfg)
	}

	cfg, err = Load([]string{dbURL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeycloakURL != "" || cfg.KillBillURL != "" || cfg.KAUIURL != "" {
		t.Fatalf("component settings must default to empty: %+v", cfg)
	}
}

func TestComponentSettingsAreValidatedWithoutValues(t *testing.T) {
	_, err := Load([]string{
		dbURL,
		"ZS_KEYCLOAK_URL=ftp://user:pw-keycloak@host",
		"ZS_KILLBILL_URL=not a url",
		"ZS_KAUI_URL=http://",
		"ZS_KEYCLOAK_ADMIN_USER=admin",
		"ZS_KILLBILL_ADMIN_PASSWORD=pw-killbill",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"ZS_KEYCLOAK_URL", "ZS_KILLBILL_URL", "ZS_KAUI_URL",
		"ZS_KEYCLOAK_ADMIN_USER and ZS_KEYCLOAK_ADMIN_PASSWORD must be set together",
		"ZS_KILLBILL_ADMIN_USER and ZS_KILLBILL_ADMIN_PASSWORD must be set together"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "pw-keycloak") || strings.Contains(msg, "pw-killbill") {
		t.Errorf("error leaks a value: %q", msg)
	}
}

func TestComponentURLsRejectUserInfo(t *testing.T) {
	for _, name := range []string{"ZS_KEYCLOAK_URL", "ZS_KILLBILL_URL", "ZS_KAUI_URL"} {
		_, err := Load([]string{dbURL, name + "=https://ops:pw-userinfo@host.internal"})
		if err == nil || !strings.Contains(err.Error(), name+" must not contain a user name or password") {
			t.Errorf("%s with userinfo: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "pw-userinfo") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}
