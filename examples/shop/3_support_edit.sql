-- 3_support_edit.sql: edits by support staff after the agent run.
--   docker compose exec db psql -U postgres -d shop -f /examples/shop/3_support_edit.sql
-- These writes are not the agent's, so they are not tagged, and undo keeps them.

-- Another column of a row the agent changed: undo keeps this phone number
-- and restores the plan.
update customers set phone = '555-9999' where id = 2;

-- The SAME column the agent changed. Undo keeps this value and reports a conflict.
update customers set plan = 'basic' where id = 4;

-- A row the agent never touched: undo leaves it alone.
update customers set plan = 'pro' where id = 15;
