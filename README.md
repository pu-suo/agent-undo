# agent-undo

Ctrl+Z for AI agents on Postgres. `undo` reverses the changes from one agent run on a live database and leaves everyone else's later edits alone.

If someone changed a value after the agent did, undo keeps their value and flags it for a person. It never silently overwrites it. The design is in [docs/design.md](docs/design.md).

## How it works

1. **Tag.** The agent logs in as its own role and sets `application_name` to a run ID such as `run-42`. Every table gets a trigger that fires before each of the agent's statements and writes a tag row, so in Postgres's change log (the WAL) the tag comes just before the rows that statement changes. Only the trigger can write tags. It also blocks the agent's writes while recording isn't working, and after the run is sealed for undo.
2. **Record.** `undo record` reads the WAL through logical replication and appends each run's changes, with whole before and after rows, to `receipts/<run>.jsonl`. It confirms a position to Postgres only after the receipt is on disk, so a crash can't lose or repeat a change.
3. **Undo.** For each row the run touched, undo starts from the row as it is now and walks the run's changes backwards. A column that still holds the agent's value goes back to its old value. A column someone changed since is kept and flagged. Rows it can't identify safely, such as rows in a table with no primary key, are refused.

## Demo

You'll need [Docker Desktop](https://www.docker.com/products/docker-desktop/) and Go 1.27 or later. From the repo root:

```bash
# 1. Start Postgres and build the demo shop (20 customers, 40 orders)
docker compose up -d --wait
docker compose exec db psql -U postgres -d shop -f /sql/1_setup.sql

# 2. Set up recording
export AGENT_UNDO_DB=postgres://postgres:postgres@localhost:5432/shop
go run ./cmd/undo init

# 3. Start the recorder and leave it running
go run ./cmd/undo record
```

In a second terminal (export `AGENT_UNDO_DB` there too):

```bash
# 4. A buggy agent cancels customers 1 to 10, paying ones included, and deletes their orders
docker compose exec -e PGAPPNAME=run-42 db psql -U agent_bot -d shop -f /sql/2_agent_run.sql

# 5. Meanwhile, support edits three customers
docker compose exec db psql -U postgres -d shop -f /sql/3_support_edit.sql

# 6. See what the agent changed, then what undo would do
go run ./cmd/undo show run-42
go run ./cmd/undo preview run-42
```

The preview restores 29 rows and keeps one: support changed customer 4's plan after the agent did, so undo keeps support's value. Support's new phone number for customer 2 is in a column the agent never touched, so it stays too.

```
  restore  public.customers id=1  set plan = "pro"
  restore  public.customers id=2  set plan = "pro"
  restore  public.customers id=3  set plan = "trial"
  keep     public.customers id=4
           kept: plan: someone changed it to "basic" after the agent set it to "cancelled"
  ...
  restore  public.orders id=1  insert the deleted row (id="1" customer_id="2" total="7.99" status="paid")
  ...
Rows: 29 restore, 1 keep

Nothing has changed yet. To apply this plan: undo apply --plan 3174e6bb178e run-42
```

```bash
# 7. Apply exactly the plan you previewed (use the hash your preview printed)
go run ./cmd/undo apply --plan <hash> run-42
```

`apply` seals the run first, so the database refuses any further writes from `run-42`. It then waits until the recorder has every change from the run and redoes the plan with the rows locked. If anything changed since your preview, it stops instead of applying a different plan.

To start over, run `docker compose down -v` and delete the `receipts` folder.

## Commands

| Command | What it does |
| --- | --- |
| `undo init` | Tags every table, creates the publication and replication slot, and sets up the receipts folder. Run it again after creating tables. |
| `undo check` | Lists every problem with the setup, such as an untagged table, or an agent role that could switch its trigger off. |
| `undo record` | Streams changes into receipts until stopped. It reconnects on its own after network errors. |
| `undo show [RUN]` | Lists runs, or prints one run's changes. |
| `undo preview RUN` | Shows what undo would restore, keep or refuse, in a read-only transaction. |
| `undo apply --plan HASH RUN` | Applies the previewed plan in one transaction. |
| `undo teardown` | Removes the trigger, publication, slot and `agent_undo` schema. Receipts are kept. |

The database comes from `--db`, else `AGENT_UNDO_DB`, else the usual `PG*` variables and `~/.pgpass`. Exit codes are 0 for success, 1 for an error, 2 for bad usage, and 3 when something needs a person.

## Running it for real

- **Postgres 14 or later** with `wal_level = logical`. Tested on 14 and 17. Managed services (Cloud SQL, RDS, AlloyDB) need their logical decoding flag turned on. Nothing in setup needs a superuser, but this hasn't been tried on a managed service yet.
- **Roles.** Agents log in as a role that owns no tables, can't create tables, and has no `TRUNCATE`, `REPLICATION` or superuser rights. `undo check` reports any way the agent could switch its trigger off. Run `init` as the tables' owner, `record` as a role with `REPLICATION`, and `apply` as a role that can write the tables and the `agent_undo.run` table, which holds each run's seal.
- **Every table is tagged,** so writes can't slip past through column grants, views or functions that run as their owner. The cost is a little more WAL for updates and deletes (`REPLICA IDENTITY FULL` keeps whole old rows) and a short trigger call per statement. Unlogged and foreign tables can't be recorded, so `check` reports them if the agent can write them.
- **Run the recorder as a service** (systemd, or one Kubernetes replica with a persistent volume). Agent writes are blocked while its replication slot is missing or lost, rather than going unrecorded. Set `max_slot_wal_keep_size`, so a recorder that stays down loses its slot instead of filling the database's disk.
- **Receipts hold whole rows**, which can include personal data. The folder must be private (`chmod 700`), and it needs the same backups and retention rules as the database.
- **Limits.** Undo can't reverse things outside the database, such as emails or payments. It refuses runs that used `TRUNCATE`, rows whose primary key the agent changed, and tables with their own triggers or rules, because those would fire again during undo. Undo compares values, so if a person later writes the exact value the agent wrote, undo can't tell the two apart and reverses it.

## Tests

```bash
go test -short ./...   # unit tests, no database needed
go test ./...          # everything, against the Postgres in docker-compose.yml
```

The end-to-end tests create a throwaway database per test. They cover the demo, the 16 hard cases from the prototype, a regression test for each problem found in review, and a crash test that kills the recorder with SIGKILL about 20 times while an agent writes. The undo tests compare against a twin database where the agent never ran: every difference must be flagged or refused, never silent. CI runs everything on Postgres 14 and 17.

## Layout

| Path | Contents |
| --- | --- |
| `cmd/undo` | The command line |
| `internal/setup` | `init`, `check` and `teardown`, plus the SQL they install |
| `internal/capture` | Reads the replication stream and attributes changes to runs |
| `internal/receipt` | Receipt files: writing, crash repair and reading |
| `internal/undo` | Preview and apply |
| `internal/db` | Connections with pinned output settings, so values print the same everywhere |
| `e2e` | End-to-end tests against real Postgres |
| `sql` | Demo scripts, numbered in run order |
| `prototype` | The early Python prototype, for reference |

## License

Apache 2.0
