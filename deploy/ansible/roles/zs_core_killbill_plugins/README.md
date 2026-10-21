# zs_core_killbill_plugins

## TL;DR

This role creates the tables of every plugin pinned in `components.yaml` in the Kill Bill database. Kill Bill does not create a plugin's own tables, and Core's service never writes to the Kill Bill database. So this is a deployment step. Run the role after Kill Bill is up and before any brand apply installs a plugin. It waits for Kill Bill, syncs Core's plugin mirror and runs `zs-core plugin ddl apply --all`. Every jar is checked against its sha256 in `components.yaml` first. The step is idempotent: a table that exists is never dropped or changed, and a second run reports no change. Core records each applied DDL in its `killbill_plugin_ddl` table. See [docs/modules/plugins.md](../../../../docs/modules/plugins.md).

## Variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `zs_core_killbill_plugins_command` | `["/usr/local/bin/zs-core"]` | The zs-core binary, or a complete command prefix that runs it in a container. |
| `zs_core_killbill_plugins_execution_mode` | `native` | `native` passes the role environment to zs-core. `container` requires it to be preconfigured inside the container. |
| `zs_core_killbill_plugins_workdir` | `/opt/zs-core` | Host-side directory with the deployed release's `components.yaml`. |
| `zs_core_killbill_plugins_killbill_url` | `http://127.0.0.1:8080` | Kill Bill, polled at `/1.0/healthcheck` until it answers 200. |
| `zs_core_killbill_plugins_environment` | `{}` | Native-mode command environment. It contains `ZS_DATABASE_URL`, `ZS_KILLBILL_DATABASE_URL` and, for mirror sync, `ZS_PLUGIN_MIRROR_DIR` and `ZS_PLUGIN_MIRROR_SECRET`. Keep secret values in the vault. |
| `zs_core_killbill_plugins_sync_mirror` | `true` | Sync the mirror first, so the DDL step reads jars from it. |
| `zs_core_killbill_plugins_operator` | `ansible` | Recorded with each DDL. |

## Behaviour

- The mirror sync downloads only jars the mirror lacks or that do not match their pin, and signs the mirror manifest. It reports a change when it downloaded a jar.
- The DDL step prints one JSON line per plugin. It reports a change when a line lists created tables.
- A DDL line that lists created domains also reports a change. Missing optional JSON fields remain unchanged.
- A DDL that differs from the one recorded for the same plugin version stops the role. Read both DDLs with `zs-core plugin ddl show <name>` and rerun the command by hand with `--force` if the change is intended.
- The role never installs or starts a plugin. New plugin installs through brand apply require `ZS_PLUGIN_MIRROR_URL` and `ZS_PLUGIN_MIRROR_SECRET`. KPM downloads only from the verified mirror, with no repository fallback. Starting an already-installed plugin needs no jar download.
- Configure the serving Core process with `ZS_PLUGIN_MIRROR_DIR` and the same `ZS_PLUGIN_MIRROR_SECRET` used by mirror sync and brand apply. Set `ZS_PLUGIN_MIRROR_URL` to that Core endpoint as Kill Bill reaches it, and finish mirror sync before the first install. The role environment configures its commands; it does not configure the separate Core service or later brand apply process.
- In `native` mode, the role writes `zs_core_killbill_plugins_environment` to a temporary mode-0600 file and sources it for each zs-core command. This prevents verbose Ansible transport traces from printing values. In `container` mode, that mapping must be empty and the required variables must already exist inside the container. The role does not forward variables with `docker exec --env` or `--env-file`.
- A command prefix whose first executable is `docker` or `docker-compose` requires `zs_core_killbill_plugins_execution_mode: container`. This rejects the native-mode assumption that an Ansible environment mapping reaches the container. The guard also handles Docker global flags and absolute Docker paths. Set container mode for any other wrapper that launches a container; the role does not parse arbitrary wrappers.
- `zs_core_killbill_plugins_workdir` is Ansible's host-side `chdir` for both modes. A container command prefix must set its own working directory, for example with `docker exec --workdir`; the role does not add or validate a container workdir.
- Check mode validates the execution contract but skips the mirror and DDL command block. It does not predict schema changes or create a temporary environment file.

## Example

```yaml
- hosts: zs_core
  roles:
    - role: zs_core_killbill_plugins
      vars:
        zs_core_killbill_plugins_killbill_url: http://killbill.internal:8080
        zs_core_killbill_plugins_environment:
          ZS_DATABASE_URL: "{{ vault_zs_core_database_url }}"
          ZS_KILLBILL_DATABASE_URL: "{{ vault_killbill_database_url }}"
          ZS_PLUGIN_MIRROR_DIR: /var/lib/zs-core/plugins
          ZS_PLUGIN_MIRROR_SECRET: "{{ vault_zs_core_plugin_mirror_secret }}"
          ZS_PLUGIN_MIRROR_URL: https://core.example.test
```

Replace the example mirror URL with the Core endpoint reachable from Kill Bill. Supply the same URL and secret to the later brand apply process.
