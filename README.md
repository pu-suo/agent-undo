# agent-undo

Undo one AI agent run's writes to a live PostgreSQL database, and keep everyone else's changes.

[![CI](https://github.com/pu-suo/agent-undo/actions/workflows/ci.yml/badge.svg)](https://github.com/pu-suo/agent-undo/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache_2.0-blue)](LICENSE)

Restoring a backup or a point-in-time copy removes an agent's bad writes, and every change made since by anyone else. agent-undo reads the run's changes from the write-ahead log and reverses only those, column by column. If someone changed a column after the agent did, undo keeps their value and reports a conflict.

```console
$ undo preview run-42
Undo plan for run-42 (plan b0ff8f70fe60)
  restore  public.customers id=1  set plan = "pro"
  restore  public.customers id=2  set plan = "pro"
  restore  public.customers id=3  set plan = "trial"
  keep     public.customers id=4
           kept: plan: someone changed it to "basic" after the agent set it to "cancelled"
  ...
  restore  public.orders id=1  insert the deleted row (id="1" customer_id="2" total="7.99" status="paid")
  ...
Rows: 29 restore, 1 keep

Nothing has changed yet. To apply this plan: undo apply --plan b0ff8f70fe60 run-42
```

- Runs on stock PostgreSQL 15 to 18, with no extension, fork or proxy in the write path.
- Detects conflicts per column, so a later edit to another column of the same row survives.
- `apply` recomputes the plan under row locks and stops if it no longer matches the preview.
- Records every change exactly once, even when the recorder is killed mid-run.
- Agent writes fail while the replication slot is missing or lost.

Any client that connects as the agent role with a run ID is recorded the same way, so a bad script can be undone too.

## Quick start

You need Docker and Go 1.27 or later.

```bash
# Start PostgreSQL and create an example shop with 20 customers and 40 orders
docker compose up -d --wait
docker compose exec db psql -U postgres -d shop -f /examples/shop/1_setup.sql

# Tag every table, create the replication slot and the receipts directory
export AGENT_UNDO_DB=postgres://postgres:postgres@localhost:5432/shop
go run ./cmd/undo init

# Start the recorder and leave it running
go run ./cmd/undo record
```

In a second terminal, with the same `AGENT_UNDO_DB`:

```bash
# Run 42 cancels customers 1 to 10, paying ones included, and deletes their orders
docker compose exec -e PGAPPNAME=run-42 db psql -U agent_bot -d shop -f /examples/shop/2_agent_run.sql

# Meanwhile support edits customers 2, 4 and 15
docker compose exec db psql -U postgres -d shop -f /examples/shop/3_support_edit.sql

# Inspect the run, preview the undo, then apply exactly that plan
go run ./cmd/undo show run-42
go run ./cmd/undo preview run-42
go run ./cmd/undo apply --plan PLAN_HASH run-42   # PLAN_HASH: the hash that preview printed
```

The preview restores 29 rows. It keeps customer 4's plan, which support changed after the agent did, and reports it as a conflict. Support's new phone number for customer 2 is in a column the agent never changed, so it stays as well. After `apply`, the database rejects further writes from `run-42`.

Run `docker compose down -v` and delete `receipts/` to start over.

## Installation

```bash
go install github.com/pu-suo/agent-undo/cmd/undo@latest
```

Prebuilt binaries for Linux, macOS and Windows are on the [releases page](https://github.com/pu-suo/agent-undo/releases).

## Connecting an agent

The agent logs in as its own role and sets `application_name` to a run ID: `run-` followed by lowercase letters, digits, `.`, `_` or `-`. Writes without a valid run ID fail.

```bash
psql "postgres://agent_bot@db.example.com/shop?application_name=run-42"   # any libpq client
PGAPPNAME=run-42 psql -U agent_bot -d shop                                 # the same, through the environment
```

Every driver can set `application_name` when it connects. Use one run ID per connection. If a pooled connection changes it with `SET application_name`, the statements after that belong to the new run.

## How it works

```mermaid
flowchart LR
    agent["AI agent<br/>application_name = run-42"] -->|SQL| pg[(PostgreSQL)]
    people["People and other apps"] -->|SQL| pg
    pg -->|logical replication, pgoutput| record["undo record"]
    record -->|fsync, then acknowledge the LSN| receipts["receipts/run-42.jsonl"]
    receipts --> undo["undo preview / apply"]
    undo -->|restore, keep or refuse| pg
```

1. **Tag.** `undo init` installs a `BEFORE ... FOR EACH STATEMENT` trigger on every table. For each statement from the agent role, it writes a row to `agent_undo.tag`, so the tag precedes that statement's changes in the write-ahead log (WAL). Only the trigger can write tags. It also rejects agent writes while the replication slot is missing or lost, and after a run is sealed for undo.
2. **Record.** `undo record` streams the WAL through logical decoding (`pgoutput`) and appends each change that follows a tag to that run's receipt, with whole before and after rows (`REPLICA IDENTITY FULL`). It acknowledges each transaction's end LSN to the replication slot only after the receipt lines are fsynced. After a crash it repairs a torn receipt tail and skips transactions it already has.
3. **Undo.** For each row the run touched, undo starts from the live row and walks the run's changes from newest to oldest, comparing exact text. A column that still holds the agent's value is restored. A column someone changed since is kept and reported as a conflict. Rows that can't be matched safely, such as rows in a table without a primary key, are refused.

`preview` runs in a read-only transaction. `apply` seals the run, waits for the recorder to reach the current WAL position, recomputes the plan with the rows locked, and writes it in one transaction after checking that it matches the preview's hash.

The design, with the alternatives considered, is in [docs/design.md](docs/design.md). The receipt format is specified in [docs/receipt-format.md](docs/receipt-format.md).

## Commands

| Command | What it does |
| --- | --- |
| `undo init` | Tags every table, creates the publication and replication slot, and sets up the receipts directory. Run it again after creating tables. |
| `undo check` | Reports problems with the setup, such as an untagged table or an agent role that could disable its trigger. |
| `undo record` | Streams changes into receipts until stopped. Reconnects after network errors. |
| `undo show [RUN]` | Lists runs, or prints one run's changes. |
| `undo preview RUN` | Shows which rows undo would restore, keep or refuse, without changing anything. |
| `undo apply --plan HASH RUN` | Applies the previewed plan in one transaction. |
| `undo teardown` | Removes the triggers, publication, replication slot and `agent_undo` schema. Receipts are kept. |

The connection string comes from `--db`, then `AGENT_UNDO_DB`, then the standard `PG*` variables and `~/.pgpass`. Exit codes: 0 success, 1 error, 2 invalid usage, 3 the plan has conflicts or refused rows that require manual review.

## Production use

- **PostgreSQL** 15 or later with `wal_level = logical`. CI covers 15 to 18. Managed services (Cloud SQL, AlloyDB, Amazon RDS) need their logical decoding setting enabled. Setup needs no superuser, but it has not yet been tested on a managed service.
- **Roles.** The agent role owns no tables and has no `CREATE`, `TRUNCATE`, `REPLICATION` or superuser rights; `undo check` reports any of these. Run `init` as the tables' owner, `record` as a role with `REPLICATION`, and `apply` as a role that can write the tables and `agent_undo.run`.
- **The recorder** runs as a long-lived service, such as a systemd unit or a single Kubernetes replica with a persistent volume. Set `max_slot_wal_keep_size` so that a stopped recorder can't fill the database's disk.
- **Receipts** contain whole rows, which can include personal data. The receipts directory must be mode 0700. Apply the database's backup and retention rules to it.
- **Cost.** `REPLICA IDENTITY FULL` adds WAL for every update and delete, and each statement on a tagged table runs a short trigger.

Roles, monitoring and recovery are covered in [docs/operations.md](docs/operations.md).

## Limitations

- Undo reverses database writes only, not their effects elsewhere, such as emails or payments.
- Runs that used `TRUNCATE` are refused: the WAL does not keep the removed rows.
- Rows whose primary key the agent changed, and rows in tables without a primary key, are refused.
- Tables with user triggers or rules are refused, because those would run again during undo.
- Changes to unlogged and foreign tables are not in the WAL, so they can't be recorded. `undo check` reports such tables if the agent can write them.
- Comparison is by value. If someone later writes back the exact value the agent wrote, undo can't tell the two apart and restores the old value.
- If the replication slot is lost, `undo init --new-epoch` starts recording again and records the gap. Runs that started before the gap are refused.

## Testing

```bash
go test -short ./...   # unit tests, no database needed
go test ./...          # all tests, against the PostgreSQL from docker-compose.yml
```

Each end-to-end test creates its own database and replication slot.

- **Differential tests** run 16 scenarios twice: against a recorded database where the agent runs and is then undone, and against a control database that never sees the agent's writes. Every difference between the two must come with a reported conflict or refusal. The scenarios cover cascading deletes, user triggers, unique collisions, large (TOASTed) values, JSON, arrays and binary data, generated columns, partitions, concurrent DDL, primary key changes and `TRUNCATE`.
- **Randomized differential test:** generates random agent runs with people's edits mixed in, undoes each run, and compares the result with a control database. Across 4,000 runs on PostgreSQL 15 to 18, no row differed from the control without undo reporting it. Results and commands are in [docs/benchmarks.md](docs/benchmarks.md).
- **Crash test:** an agent makes 600 single-row commits while the recorder, a separate process, is SIGKILLed at random 50 to 300 ms intervals. Every row must land in the receipt exactly once.
- **Regression tests** cover each fixed bug, such as transactions that began before a seal, and deletes that would cascade to other people's rows.

CI runs the tests with the race detector on PostgreSQL 15, 16, 17 and 18, plus golangci-lint and govulncheck.

## Related projects

- [pgbrake](https://github.com/lexang/pgbrake) routes agent writes through its own server and undoes them one write at a time.
- [EterDB](https://github.com/eterdb/eterdb) is a modified PostgreSQL 18 engine that undoes transactions and can track reads.

The comparison is in [docs/design.md](docs/design.md#related-work).

## Status

v0.1 is the command-line tool. Planned next:

- MCP tools for agents (`show_changes` and `preview_undo`, then `request_undo` with human approval).
- Testing on Cloud SQL, AlloyDB and Amazon RDS.
- A replay step that finds people's writes that depended on the agent's bad data.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). To report a security issue, see [SECURITY.md](SECURITY.md).

## License

Apache 2.0. See [LICENSE](LICENSE).
