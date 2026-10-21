-- The plugin DDLs zs-core plugin ddl apply ran against the Kill Bill
-- database. Kill Bill does not create a plugin's own tables, so Core applies
-- the ddl.sql of the pinned jar once, as a deployment step, and records it
-- here. One row per plugin, version and SHA-256 of the DDL text. A second
-- DDL for the same plugin version is refused unless the operator forces it.
-- tables are every table the DDL creates and created_tables those the last
-- apply created. See docs/modules/plugins.md.
CREATE TABLE killbill_plugin_ddl (
    plugin text NOT NULL,
    version text NOT NULL,
    ddl_sha256 text NOT NULL,
    tables text[] NOT NULL,
    created_tables text[] NOT NULL,
    applied_by text NOT NULL,
    forced boolean NOT NULL DEFAULT false,
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin, version, ddl_sha256),
    CONSTRAINT killbill_plugin_ddl_plugin CHECK (plugin ~ '^[a-z0-9][a-z0-9-]{0,63}$'),
    CONSTRAINT killbill_plugin_ddl_version_length CHECK (char_length(version) BETWEEN 1 AND 50),
    CONSTRAINT killbill_plugin_ddl_digest CHECK (ddl_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT killbill_plugin_ddl_applied_by_length CHECK (char_length(applied_by) BETWEEN 1 AND 255)
);
