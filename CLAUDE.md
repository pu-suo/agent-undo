# agent-undo: notes for Claude Code

## What this project is
Ctrl+Z for AI agents on Postgres. When an agent makes bad writes, `undo` reverses
exactly that agent run's changes on the live database and keeps everyone else's
later edits. Full design: docs/design.md. Demo: README.md.

## How to work with me
- I'm new to backend development, Go and Postgres internals. Explain each new idea
  in plain words before writing code that uses it.
- This is my portfolio project for Google SWE intern team matching, and I must be
  able to explain every line in an interview. So:
  - Core logic (internal/capture, internal/receipt, internal/undo): guide me and let
    me write it. Show small examples, review what I write, point out bugs. Don't
    write whole files of core logic unless I ask.
  - Plumbing (Docker, CI, Makefile, test scaffolding): you may write it, but tell
    me what each part does.
- Keep steps small: one idea, one change, run it, then commit with a clear message.
- After each step, ask me one short question to check I understood it.

## How it works (v1)
1. Tag. The agent logs in as `agent_bot` and sets `application_name` to its run ID
   (for example run-42). The statement trigger `label_agent_write` (SECURITY
   DEFINER) calls `pg_logical_emit_message(true, 'agent_run', <run ID>)`, so the
   run ID lands in the WAL inside the agent's transaction. EXECUTE on
   pg_logical_emit_message is revoked from PUBLIC, so nobody else can forge a tag.
   Setup lives in sql/1_setup.sql.
2. Record. A Go service streams changes over logical replication with
   github.com/jackc/pglogrepl, plugin `pgoutput`, options `proto_version '2'`,
   `publication_names 'undo_pub'`, `messages 'true'`. Start from pglogrepl's
   example/pglogrepl_demo/main.go. Tables use REPLICA IDENTITY FULL, so UPDATE and
   DELETE carry the full old row. A transaction that contains an `agent_run`
   message belongs to that run; transactions without one are people's work.
3. Receipt. Append each change of a run (LSN, table, operation, key, old row,
   new row) to receipts/<run>.jsonl. Confirm an LSN to Postgres (standby status
   update) only AFTER its receipt line is fsynced. After a restart Postgres
   resends unconfirmed changes, so skip LSNs already in the receipt.
4. Undo. Walk the receipt from newest change to oldest. Compare each change with
   the row as it is now, column by column:
   - restore: the column still holds the agent's value, so put the old value back
   - flag: someone changed that same column since; keep their value and report it
   - refuse: no safe way back (for example, a table without a primary key)
   Rule: never silently overwrite someone else's later edit.

## Decisions already made
- Go, latest stable; module path github.com/pu-suo/agent-undo.
- Postgres 17 in Docker (anything 14+ works).
- Receipts are JSON Lines files, one per run.
- v1: only a person can apply an undo. Agent tools come over MCP after the CLI
  works: show_changes and preview_undo first, request_undo with approval later.
- v1 does not handle DROP or TRUNCATE: agent_bot is simply not granted them.
- The test_decoding slot `peek` in sql/1_setup.sql is only for looking by hand.

## Layout (planned; create folders as they're needed)
- cmd/undo/          command line: record, show, preview, apply
- internal/capture/  replication stream to changes
- internal/receipt/  read and write receipts
- internal/undo/     restore, flag and refuse rules
- sql/               demo scripts, run in number order (see README)
- prototype/         Python prototype and the 16 hard cases to port (reference only)

## Commands
- Start the database: `docker compose up -d`. Wipe and start fresh: `docker compose down -v`
- Run the demo: the numbered steps in README.md
- Connect from Go: postgres://postgres:postgres@localhost:5432/shop
- Replication connection from Go: the same URL plus `?replication=database`
- Tests: `go test ./...` (the database must be running)

## Milestones
- Nov 15 checkpoint: the recorder prints the agent's changes with run ID and old
  and new values, and writes receipts; `undo show run-42` prints a receipt; one
  end-to-end test passes. If Go isn't working by then, switch v1 to Python.
- Nov 29, v1: `undo preview` and `undo apply` with the restore, flag and refuse
  rules; all 16 hard cases pass or are safely refused; a crash test; a README demo.
