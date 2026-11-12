-- 2_agent_run.sql: a simulated agent run with a bug. Run it as the agent role:
--   docker compose exec -e PGAPPNAME=run-42 db psql -U agent_bot -d shop -f /examples/shop/2_agent_run.sql
-- PGAPPNAME sets the connection's application_name, which is the run ID.
--
-- The task was to cancel expired trial accounts. The bug: it never checks
-- the plan, so it cancels customers 1 to 10, paying ones included, and then
-- deletes their orders.

update customers set plan = 'cancelled' where id <= 10;

delete from orders
where customer_id in (select id from customers where plan = 'cancelled');
