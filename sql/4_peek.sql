-- 4_peek.sql: look at what Postgres recorded, without removing it from the log.
--   docker compose exec db psql -U postgres -d shop -f /sql/4_peek.sql
--
-- What to look for:
--   * BEGIN ... COMMIT wraps each transaction.
--   * "message: ... prefix: agent_run ... content:run-42" marks the agent's
--     transactions. The support edits have no such message.
--   * UPDATE and DELETE lines show the old values (old-key) and the new values
--     (new-tuple). Those old values are exactly what undo needs to put things back.

select data from pg_logical_slot_peek_changes('peek', null, null);
