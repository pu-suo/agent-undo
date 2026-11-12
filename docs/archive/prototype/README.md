# Prototype (archived)

The Python prototype that tested the idea before the Go implementation. It is kept for reference and is not maintained. It decodes the WAL with `test_decoding` rather than `pgoutput`, and tags agent writes with a statement trigger that calls `pg_logical_emit_message`.

- `agent_undo_prototype.py`: tagging, recording and column-by-column undo in one file.
- `undo_scorecard.py`: 16 scenarios, each run against a control database that never sees the agent's writes. They cover cascading deletes, triggers, unique collisions, large values, key changes, tables without keys, partitions, dropped columns, TRUNCATE and DROP.

The prototype's last result on these scenarios: 3 correct, 2 reported for review, 3 wrong without any warning, and 8 crashed. The Go implementation runs the same 16 scenarios in `e2e/hardcases_test.go`. There, 10 match the control database, and 6 are refused or reported as conflicts.
