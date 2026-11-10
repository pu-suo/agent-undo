# agent-undo

Ctrl+Z for AI agents on Postgres. `undo` reverses the changes from one agent run on a live database and leaves everyone else's later edits alone.

This is early work. The `undo` command isn't built yet, so the demo below stops at showing what Postgres records for an agent run. The design is in [docs/design.md](docs/design.md).

## Demo

You'll need [Docker Desktop](https://www.docker.com/products/docker-desktop/) running. From the repo root:

```bash
# 1. Start Postgres
docker compose up -d

# 2. Create the demo shop and the trigger that labels agent writes
docker compose exec db psql -U postgres -d shop -f /sql/1_setup.sql

# 3. Run a fake agent (run-42) with a bug: it cancels customers 1 to 10,
#    paying ones included, and deletes their orders
docker compose exec -e PGAPPNAME=run-42 db psql -U agent_bot -d shop -f /sql/2_agent_run.sql

# 4. A support person edits some customers afterwards
docker compose exec db psql -U postgres -d shop -f /sql/3_support_edit.sql

# 5. Show what Postgres recorded
docker compose exec db psql -U postgres -d shop -f /sql/4_peek.sql
```

To poke around, open a shell with `docker compose exec db psql -U postgres -d shop` and try `select * from customers order by id;`. `\q` quits.

`docker compose down -v` wipes the database. Start again from step 1 after that.

## Layout

| Path | Contents |
| --- | --- |
| `docker-compose.yml` | Postgres 17 with `wal_level=logical` |
| `sql/` | Demo scripts, numbered in run order |
| `docs/design.md` | Design notes |
| `prototype/` | Early Python prototype and the 16 hard test cases |

## License

Apache 2.0
