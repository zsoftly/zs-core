# Configuration

## TL;DR

ZSoftly Core reads its configuration from environment variables only, there is no configuration file, and every variable starts with `ZS_`. The service validates all of them at start and refuses to run if any value is wrong, listing every problem at once. An unknown `ZS_` variable is also an error, so a typo cannot fall back to a default without anyone noticing. Error messages name the variable but never repeat its value. `ZS_DATABASE_URL` is always required, and `serve` also needs `ZS_KEYCLOAK_URL` to verify tokens. The admin settings are optional for `serve`. Brand apply and `doctor` need them. Each URL must be `http://` or `https://` with a host, and a URL with a user name or password is an error. `.env.example` lists every variable with its default.

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
| `ZS_HTTP_WRITE_TIMEOUT` | Go duration | `30s` | no | Time allowed to write a response. `POST /v1/brands/{id}/apply` extends its own deadline, because it runs for up to 10 minutes. |
| `ZS_HTTP_IDLE_TIMEOUT` | Go duration | `120s` | no | Keep-alive idle timeout. |
| `ZS_SHUTDOWN_TIMEOUT` | Go duration | `15s` | no | Time allowed for in-flight requests after SIGTERM or SIGINT. |
| `ZS_KEYCLOAK_URL` | `http(s)://host[:port]` | none | for `serve` | Base URL of Keycloak as the service reaches it. `serve` fetches each brand realm's discovery document and signing keys from it. Also used by brand apply and `doctor`. |
| `ZS_KEYCLOAK_REALM` | text | none | no | A realm that `zs-core doctor` must find, such as `zs-reference`. |
| `ZS_KEYCLOAK_ADMIN_USER` | text | none | no | Keycloak administrator. Set together with the password. Without it, brand apply fails and `doctor` reports a skip. |
| `ZS_KEYCLOAK_ADMIN_PASSWORD` | text | none | no | Password of that administrator. Never logged. |
| `ZS_KEYCLOAK_ADMIN_CLIENT_ID` | text | none | no | A confidential client in the `master` realm with realm management rights. When set with its secret, the Admin API token comes from the client credentials grant instead of the admin password. |
| `ZS_KEYCLOAK_ADMIN_CLIENT_SECRET` | text | none | no | Secret of that client. Set together with the client ID. Never logged. |
| `ZS_KILLBILL_URL` | `http(s)://host[:port]` | none | no | Base URL of Kill Bill. Used by brand apply, brand diff and `doctor`. |
| `ZS_KILLBILL_ADMIN_USER` | text | none | no | Kill Bill administrator, normally `admin`. Set together with the password. Without it, brand apply fails and `doctor` reports a skip. |
| `ZS_KILLBILL_ADMIN_PASSWORD` | text | none | no | Password of that administrator. Never logged. |
| `ZS_KAUI_URL` | `http(s)://host[:port]` | none | no | Base URL of KAUI. When unset, `doctor` skips the KAUI check. |
| `ZS_BRANDS_DIR` | directory | none | no | Directory of brand manifests, `<slug>.yaml`. `serve` validates every manifest at start, secret references included, and refuses to start on an invalid one. It logs any manifest that differs from what was last applied. Apply stays an explicit step. File references in `POST /v1/brands/{id}/apply` of brand `<slug>` must start with `<slug>/` and resolve inside `ZS_BRANDS_DIR/<slug>/`. See [modules/brands.md](modules/brands.md). |
| `ZS_SECRETS_DIR` | absolute directory | none | no | Directory of file secrets for brand manifests, one subdirectory per brand slug. A `file://` reference of brand `<slug>` must resolve inside `ZS_SECRETS_DIR/<slug>/`. When unset, every `file://` reference is refused. See [modules/brands.md](modules/brands.md#secrets). |

Booleans accept exactly `true` or `false`. Durations use Go syntax, such as `500ms`, `30s` or `2m`, and must be positive.

## Component settings

`serve` needs `ZS_KEYCLOAK_URL`. It reads the issuer and signing keys of every applied brand's realm from that URL. Core never fetches keys from a URL a token names. Without the Keycloak and Kill Bill admin settings, `serve` still verifies tokens, and `POST /v1/brands/{id}/apply` answers `503 not_ready`.

`zs-core brand apply` and `brand diff` need three settings: `ZS_KEYCLOAK_URL`, Keycloak admin credentials (a user and password, or a client ID and secret), and `ZS_KILLBILL_URL` with the Kill Bill admin user and password. `migrate` ignores the component settings. `zs-core doctor` uses them to check that a running stack matches `components.yaml`, and needs `ZS_DATABASE_URL` for the database check. Each URL must be `http://` or `https://` with a host. A URL with a user name or password is a configuration error. Put credentials in the `*_ADMIN_USER` and `*_ADMIN_PASSWORD` settings. An admin user without its password, a client ID without its secret, or the reverse, is a configuration error.

`doctor` reads `components.yaml` from the working directory. Pass `--components <file>` to use another path. The Core container image carries the file in its working directory, so `docker compose exec core zs-core doctor` needs no flag.

Variables that start with `ZS_TEST_` are reserved for the test suite. The service ignores them. They are described in [development.md](development.md).

Secret references in brand manifests are not `ZS_` variables. `ZS_` is the configuration namespace. An `env://` reference of brand `<slug>` must start with `BRAND_<SLUG>__`, the slug in upper case with `-` replaced by `_`, followed by two underscores. The reference brand uses `BRAND_ZS_REFERENCE__`. A brand `zs` would use `BRAND_ZS__`, so neither brand can name the other's variables. Apply refuses any other name with `422 secret_ref_invalid`.
