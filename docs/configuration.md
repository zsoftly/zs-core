# Configuration

## TL;DR

ZSoftly Core reads its configuration from environment variables only. There is no configuration file. Every variable starts with `ZS_`. The service validates all of them at start and refuses to run if any value is wrong, listing every problem at once. An unknown `ZS_` variable is also an error, so a typo cannot fall back to a default without anyone noticing. Error messages name the variable but never repeat its value. Only `ZS_DATABASE_URL` is required. The component settings used by `zs-core doctor` are optional for the service itself. `.env.example` lists every variable with its default.

## Variables

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `ZS_DATABASE_URL` | Postgres URL | none | yes | Connection URL, `postgres://` or `postgresql://`, with a host. The URL may hold the password. The service never logs it. |
| `ZS_DATABASE_MAX_CONNS` | integer, 1 to 1000 | `10` | no | Maximum connections in the pool. |
| `ZS_LISTEN_ADDR` | `host:port` | `127.0.0.1:8080` | no | HTTP listen address. Use `0.0.0.0:8080` or `:8080` to listen on every interface. |
| `ZS_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` | no | Minimum log level. |
| `ZS_LOG_FORMAT` | `json`, `text` | `json` | no | Log line format on standard error. |
| `ZS_MIGRATE_ON_START` | `true`, `false` | `true` | no | Apply pending migrations before `serve` listens. When `false`, run `zs-core migrate` first. `/readyz` stays at 503 until you apply the migrations. |
| `ZS_HTTP_READ_HEADER_TIMEOUT` | Go duration | `5s` | no | Time allowed to read request headers. |
| `ZS_HTTP_READ_TIMEOUT` | Go duration | `30s` | no | Time allowed to read a whole request. |
| `ZS_HTTP_WRITE_TIMEOUT` | Go duration | `30s` | no | Time allowed to write a response. |
| `ZS_HTTP_IDLE_TIMEOUT` | Go duration | `120s` | no | Keep-alive idle timeout. |
| `ZS_SHUTDOWN_TIMEOUT` | Go duration | `15s` | no | Time allowed for in-flight requests after SIGTERM or SIGINT. |
| `ZS_DEV_TRUSTED_SCOPE_HEADERS` | `true`, `false` | `false` | no | Development only. Read the brand and tenant of `/v1` requests from the `X-ZS-Brand` and `X-ZS-Tenant` headers. Refused unless `ZS_LISTEN_ADDR` is a loopback address. See below. |
| `ZS_KEYCLOAK_URL` | `http(s)://host[:port]` | none | no | Base URL of Keycloak as the service reaches it. Used by `zs-core doctor`. |
| `ZS_KEYCLOAK_REALM` | text | none | no | A realm that `zs-core doctor` must find, such as `zs-reference`. |
| `ZS_KEYCLOAK_ADMIN_USER` | text | none | no | Keycloak administrator. Set together with the password. Without it, `doctor` cannot read the Keycloak version and reports a warning. |
| `ZS_KEYCLOAK_ADMIN_PASSWORD` | text | none | no | Password of that administrator. Never logged. |
| `ZS_KILLBILL_URL` | `http(s)://host[:port]` | none | no | Base URL of Kill Bill. Used by `zs-core doctor`. |
| `ZS_KILLBILL_ADMIN_USER` | text | none | no | Kill Bill administrator, normally `admin`. Set together with the password. Without it, `doctor` cannot read the version or the plugin list and reports a warning. |
| `ZS_KILLBILL_ADMIN_PASSWORD` | text | none | no | Password of that administrator. Never logged. |
| `ZS_KAUI_URL` | `http(s)://host[:port]` | none | no | Base URL of KAUI. When unset, `doctor` skips the KAUI check. |

Booleans accept exactly `true` or `false`. Durations use Go syntax, such as `500ms`, `30s` or `2m`, and must be positive.

## Component settings and `zs-core doctor`

`serve` and `migrate` ignore the Keycloak, Kill Bill and KAUI settings. `zs-core doctor` uses them to check that a running stack matches `components.yaml`. It also needs `ZS_DATABASE_URL` for the database check. Each URL must be `http://` or `https://` with a host. An admin user without its password, or the reverse, is a configuration error.

`doctor` reads `components.yaml` from the working directory. Pass `--components <file>` to use another path. The Core container image carries the file in its working directory, so `docker compose exec core zs-core doctor` needs no flag.

Variables that start with `ZS_TEST_` are reserved for the test suite. The service ignores them. `ZS_TEST_POSTGRES_URL` is described in [development.md](development.md).

## Trusted scope headers (Phase 1 only)

Phase 1 has no authentication. Every `/v1` request needs a brand and a tenant, and the only way to supply them is the header pair `X-ZS-Brand: brd_...` and `X-ZS-Tenant: tnt_...`. The service reads these headers only when `ZS_DEV_TRUSTED_SCOPE_HEADERS=true`. Anyone who reaches the port acts as any tenant of any brand. The service therefore refuses to start with the flag unless it listens on `127.0.0.1`, `::1` or `localhost`. It logs a warning at start.

Without the flag, every `/v1` request returns `401 unauthenticated`. `/healthz` and `/readyz` work either way.

Phase 2 removes the flag. The brand will come from the Keycloak token audience or the API key, and the tenant from the verified membership.
