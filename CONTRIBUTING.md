# Contributing

Bug reports and pull requests are welcome. For anything larger than a fix, please open an issue first to discuss the approach.

## Development setup

You need Go 1.27 or later and Docker.

```bash
docker compose up -d --wait   # PostgreSQL 17 with wal_level=logical, on 127.0.0.1:5432
go test -short ./...          # unit tests, no database needed
go test -race ./...           # everything, including the end-to-end tests
```

The end-to-end tests connect as an admin to the server in `AGENT_UNDO_TEST_DB` (default `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`). Each test creates its own database and replication slot and removes them afterwards. To test another PostgreSQL version, run `docker compose down -v && PG_VERSION=16 docker compose up -d --wait`.

CI also runs [golangci-lint](https://golangci-lint.run) with the settings in `.golangci.yml`:

```bash
golangci-lint run
```

## Layout

| Path | Contents |
| --- | --- |
| `cmd/undo` | The command-line tool: argument parsing and output |
| `internal/setup` | `init`, `check` and `teardown`, and the SQL they install (`install.sql`) |
| `internal/capture` | Reads the replication stream and attributes changes to runs |
| `internal/receipt` | Receipt files: writing, crash repair and reading |
| `internal/undo` | Preview and apply |
| `internal/db` | Connections with pinned output settings |
| `e2e` | End-to-end tests against a real PostgreSQL |
| `examples/shop` | The quick-start database and scripts |
| `docs` | Design, receipt format and operations |
| `docs/archive/prototype` | The original Python prototype, kept for reference |

## Invariants

Changes must keep these properties, and the tests check them:

- The recorder acknowledges an LSN only after everything up to it is fsynced.
- Undo never writes a column that someone changed after the agent: it keeps the value and reports a conflict.
- `apply` writes exactly the plan that `preview` showed, or nothing.

## Pull requests

- Keep each pull request to one change, with tests for any change in behavior.
- Run `gofmt`, `go vet` and the full test suite before opening it.
- Write the title in the [Conventional Commits](https://www.conventionalcommits.org/) style, for example `fix(undo): keep rows a cascade would change`. Types in use: `feat`, `fix`, `docs`, `test`, `refactor`, `ci`, `chore`.
