-- 3_support_edit.sql: meanwhile, a support person keeps working.
--   docker compose exec db psql -U postgres -d shop -f /sql/3_support_edit.sql
-- These writes are not the agent's, so they get no label, and undo must keep them.

-- A different column on a row the agent changed.
-- Undo should keep this new phone AND put the plan back.
update customers set phone = '555-9999' where id = 2;

-- The SAME column the agent changed. Undo must not overwrite this: it should flag it.
update customers set plan = 'basic' where id = 4;

-- A row the agent never touched. Undo should leave it completely alone.
update customers set plan = 'pro' where id = 15;
