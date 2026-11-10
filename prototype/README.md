# Prototype (reference only)

This is the early Python prototype used to test the idea before the real build. It reads changes with the `test_decoding` format instead of `pgoutput`, and it is not the real implementation.

- `agent_undo_prototype.py`: tag, record and undo in one file, with column-by-column undo.
- `undo_scorecard.py`: the 16 hard cases (cascading deletes, triggers, unique collisions, large values, changed keys, tables without keys, partitions, dropped columns, TRUNCATE, DROP and more). Each case runs against a second "twin" database where the agent never ran.

Last scorecard result: 3 correct, 2 flagged, 3 silently wrong, 8 crashed. These cases are to be ported into Go tests for v1, where every case must pass or be safely refused.
