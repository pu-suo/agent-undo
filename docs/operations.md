# Operations

How to run agent-undo against a production database: requirements, roles, running the recorder as a service, monitoring, and recovery.

## Requirements

- PostgreSQL 15 or later. CI tests 15, 16, 17 and 18.
- `wal_level = logical`, which needs a restart. On Cloud SQL, AlloyDB and Amazon RDS, enable the provider's logical decoding setting. agent-undo has not yet been tested on a managed service.
- One free replication slot and one free WAL sender (`max_replication_slots`, `max_wal_senders`).
- `max_slot_wal_keep_size` set to a limit your disk can hold. Without it, a recorder that stays stopped makes PostgreSQL keep WAL until the disk is full.
- `server_encoding = UTF8`.

`undo check` verifies all of these and reports every problem it finds.

## Roles

Use a separate role for each job. None of them needs to be a superuser.

| Role | Used by | Needs |
| --- | --- | --- |
| Agent | AI agents and scripts | `LOGIN`, and `SELECT`, `INSERT`, `UPDATE`, `DELETE` on the tables it works on. It must own no tables and have no `CREATE`, `TRUNCATE`, `TRIGGER`, `REPLICATION` or superuser rights, directly or through roles it can `SET ROLE` to. |
| Owner | `undo init`, `undo teardown` | Owner of the tables, with `CREATE` on the database |
| Recorder | `undo record` | `LOGIN` and `REPLICATION`; usage of schema `agent_undo` and `SELECT` on `agent_undo.settings` |
| Apply | `undo preview`, `undo apply` | `SELECT`, `INSERT`, `UPDATE`, `DELETE` on the tables; usage of schema `agent_undo` and `SELECT`, `INSERT`, `UPDATE` on `agent_undo.run`. It must bypass row-level security on tables that use it. |

For example, after `undo init`:

```sql
create role undo_recorder login replication;
create role undo_apply login;
grant usage on schema agent_undo to undo_recorder, undo_apply;
grant select on agent_undo.settings to undo_recorder;
grant select, insert, update on agent_undo.run to undo_apply;
grant select, insert, update, delete on all tables in schema public to undo_apply;
```

`undo apply` refuses to run as the agent role, because the trigger would tag its writes as a new run.

## Connecting

Each command takes a connection string from `--db`, then `AGENT_UNDO_DB`, then the standard `PG*` variables, service file and `~/.pgpass`. Keep passwords out of the command line: use a password file (`PGPASSFILE`) readable only by the service user. For a server on another host, use `sslmode=verify-full`. agent-undo logs a warning when a remote connection doesn't verify the server.

Every connection agent-undo opens pins the output settings that receipts depend on, and an empty `search_path`.

## Running the recorder

Run one recorder per database, as a long-lived service with a persistent, private receipts directory. With systemd:

```ini
[Unit]
Description=agent-undo recorder
After=network-online.target
Wants=network-online.target

[Service]
User=agent-undo
Environment=AGENT_UNDO_DB=postgres://undo_recorder@db.example.com/shop?sslmode=verify-full
Environment=PGPASSFILE=/etc/agent-undo/pgpass
ExecStart=/usr/local/bin/undo record --receipts /var/lib/agent-undo/receipts
Restart=always
RestartSec=5
StateDirectory=agent-undo
StateDirectoryMode=0700
UMask=0077

[Install]
WantedBy=multi-user.target
```

On Kubernetes, run a single replica with a persistent volume for the receipts. Never keep receipts on a container's writable layer: once a position is acknowledged, PostgreSQL won't send its changes again.

The recorder reconnects with backoff after network errors, server restarts and cancelled queries. It stops on errors a retry can't fix, such as a missing slot, a failed `fsync` or a full receipts disk. It handles SIGTERM by acknowledging what is on disk and exiting.

Run `undo init` again after creating tables, so they are tagged too. `undo check` reports any table that isn't.

## Monitoring

Alert on these:

- The slot is inactive, or its lag grows:

  ```sql
  select active, wal_status, safe_wal_size,
         pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)) as behind
  from pg_replication_slots
  where slot_name = 'undo_slot';
  ```

- `wal_status` is `unreserved` (WAL is about to be removed) or `lost` (the slot was invalidated).
- `undo check` exits non-zero.
- The recorder logs at `ERROR`, for example "recorded an incomplete transaction".
- Free space on the receipts volume.

## Recovery

- **Recorder down.** Agent writes continue, and their changes wait in the slot. Restart the recorder and it catches up. `undo apply` waits for the recorder, and fails if it hasn't caught up.
- **Slot lost or moved.** Agent writes fail while the slot is missing or lost. Run `undo init --new-epoch` to create a new slot and record the gap. Runs that started before the gap are refused.
- **Database restored from a backup.** The recorder refuses to start, because the server's WAL ends before the receipts. Start a new receipts directory with `undo init`.
- **Failover and major upgrades.** A logical slot does not move to a new primary, and `pg_upgrade` before PostgreSQL 17 drops it. Afterwards, run `undo init --new-epoch`.

## Receipts and personal data

Receipts hold whole before and after rows of every row an agent changed, including rows it deleted. Store them on an encrypted volume. Back them up and delete them under the same rules as the database. A run whose receipt is deleted can no longer be undone.

## Removing agent-undo

Stop the recorder, then run `undo teardown`. It drops the replication slot first, and refuses while a recorder is connected. It then removes the triggers, the publication and the `agent_undo` schema. Receipts stay on disk, and tables keep `REPLICA IDENTITY FULL`.
