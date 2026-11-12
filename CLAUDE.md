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

## How it works (v1, as built)
1. Tag. `undo init` installs schema `agent_undo` (internal/setup/install.sql) and a
   BEFORE ... FOR EACH STATEMENT trigger `agent_undo_tag` on EVERY permanent table
   outside system schemas (not just tables the agent can write: column grants,
   views, SET ROLE and definer functions would otherwise escape). Partitioned
   roots are published with ONLY, so later partitions are published too. Before
   each agent statement, the SECURITY DEFINER function
   `agent_undo.tag_statement()` checks the run ID
   (`^run-[a-z0-9][a-z0-9._-]{0,58}$`, from application_name), refuses writes
   unless the slot exists in this database and is ready (confirmed_flush_lsn not
   null) and not lost, records the run in `agent_undo.run` (first_lsn), takes a
   FOR SHARE lock on that row and refuses sealed runs, then upserts
   `agent_undo.tag` (only the trigger can write it, so tags can't be forged). In
   the WAL the tag row comes just BEFORE the statement's changes. (The first
   design used pg_logical_emit_message in an AFTER trigger; changed because
   REVOKE on it needs a superuser, and tag-first lets the recorder stream without
   buffering anyone's transactions.)
2. Record. `undo record` (internal/capture) streams pgoutput (`proto_version '2'`,
   the publication from state.json). A change after a tag belongs to that tag's run
   (statement by statement); changes before any tag are people's and are dropped.
   Tables use REPLICA IDENTITY FULL, so UPDATE and DELETE carry the whole old row;
   an unchanged large value ('u') is filled from the old row.
3. Receipt. receipts/<run>.jsonl (internal/receipt): "change" lines, then a
   "commit" line per transaction. Confirm COMMIT.end_lsn only after the receipt is
   fsynced and receipts/state.json is saved. A cut-off tail is truncated on open;
   after a restart, a transaction whose commit LSN is below the receipt's last
   end LSN is skipped. Keepalive positions are confirmed when no transaction is
   open, so the slot advances while agents are idle.
4. Undo. internal/undo: per row, start from the live row and walk the run's
   changes newest to oldest, comparing exact output text (never SQL `=`):
   - restore: the column still holds the agent's value, so put the old value back
   - flag (keep): someone changed it since; keep their value and report it
   - refuse: no safe way back (no primary key, primary key changed, TRUNCATE,
     incomplete receipt, tables with user triggers or rules, RLS)
   Preview is read-only and prints a plan hash. Apply seals the run (UPDATE of
   agent_undo.run, which waits for the run's open transactions via their FOR
   SHARE locks), waits until the slot AND state.json reach
   pg_current_wal_insert_lsn(), re-decides with rows locked (batched reads),
   requires the same hash, writes in one transaction (batched; on a constraint
   error, retries with a savepoint per row), reads every row back, then commits.
   Known limit: a person re-writing the agent's exact value is reverted.
   Rule: never silently overwrite someone else's later edit.

## Decisions already made
- Go, latest stable; module path github.com/pu-suo/agent-undo.
- Postgres 17 in Docker; tested on 14 and 17.
- Receipts are JSON Lines files, one per run.
- v1: only a person can apply an undo. Agent tools come over MCP after the CLI
  works: show_changes and preview_undo first, request_undo with approval later.
- v1 refuses TRUNCATE runs; DROP is impossible because the agent owns nothing.
- stdlib flag (no cobra), log/slog to stderr, data to stdout; exit codes 0/1/2/3.

## Layout
- cmd/undo/          command line: init, check, record, show, preview, apply, teardown
- internal/db/       connections with pinned output settings (empty search_path!)
- internal/setup/    install.sql, init, check, teardown
- internal/capture/  replication stream to receipt changes
- internal/receipt/  read and write receipts, state.json
- internal/undo/     restore, flag and refuse rules; preview and apply
- e2e/               end-to-end tests: demo, 16 hard cases, crash test
- sql/               demo scripts, run in number order (see README)
- prototype/         Python prototype (reference only)

## Commands
- Start the database: `docker compose up -d --wait`. Wipe: `docker compose down -v`
- Run the demo: the steps in README.md (AGENT_UNDO_DB=postgres://postgres:postgres@localhost:5432/shop)
- Tests: `go test -short ./...` (no database) or `go test ./...` (database running);
  AGENT_UNDO_TEST_DB points the e2e tests at another server.

## Milestones
- Nov 15 checkpoint: the recorder prints the agent's changes with run ID and old
  and new values, and writes receipts; `undo show run-42` prints a receipt; one
  end-to-end test passes. If Go isn't working by then, switch v1 to Python.
- Nov 29, v1: `undo preview` and `undo apply` with the restore, flag and refuse
  rules; all 16 hard cases pass or are safely refused; a crash test; a README demo.
