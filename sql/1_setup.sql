-- 1_setup.sql: run once, as the admin user (postgres):
--   docker compose exec db psql -U postgres -d shop -f /sql/1_setup.sql
--
-- Builds a tiny shop, gives the "agent" its own login, and makes Postgres
-- label every change the agent makes with its run ID.

-- 1. A tiny shop to experiment on: 20 customers, 40 orders.
create table customers (
  id    int primary key,
  name  text not null,
  plan  text not null,  -- 'trial', 'pro', 'basic' or 'cancelled'
  phone text
);

create table orders (
  id          int primary key,
  customer_id int not null references customers(id),
  total       numeric(10,2) not null,
  status      text not null
);

insert into customers
select g, 'Customer ' || g,
       case when g % 3 = 0 then 'trial' else 'pro' end,
       '555-' || lpad(g::text, 4, '0')
from generate_series(1, 20) g;

insert into orders
select g, 1 + (g % 20), (g * 7 % 100) + 0.99, 'paid'
from generate_series(1, 40) g;

-- 2. Keep complete "before" values in the change log, not just the ID.
alter table customers replica identity full;
alter table orders    replica identity full;

-- 3. The agent gets its own login, so its writes can be told apart.
create role agent_bot login;
grant select, insert, update, delete on customers, orders to agent_bot;

-- 4. Only our trigger may write labels into the change log.
--    By default ANY user can call pg_logical_emit_message, so anyone could
--    forge a run label. Take that right away from everyone.
do $$
declare f regprocedure;
begin
  for f in select oid::regprocedure from pg_proc where proname = 'pg_logical_emit_message' loop
    execute format('revoke execute on function %s from public', f);
  end loop;
end $$;

-- 5. The labeling trigger. After any statement run by agent_bot, it writes a
--    message into the change log: prefix 'agent_run', content = the run ID.
--    The run ID is the connection's application_name, which the agent sets
--    (for example run-42).
--    SECURITY DEFINER means it runs with its creator's rights, so it can still
--    call pg_logical_emit_message after step 4.
create function label_agent_write() returns trigger
language plpgsql security definer set search_path = pg_catalog as $$
begin
  if session_user = 'agent_bot' then
    perform pg_logical_emit_message(true, 'agent_run', current_setting('application_name'));
  end if;
  return null;
end $$;

create trigger label_agent_write
  after insert or update or delete on customers
  for each statement execute function label_agent_write();

create trigger label_agent_write
  after insert or update or delete on orders
  for each statement execute function label_agent_write();

-- 6. Readers for the change log.
--    'peek' uses the human-readable test_decoding format, for looking by hand.
--    An unread slot makes Postgres keep old log files forever. That's fine for
--    this demo, but drop it when you're done with it:
--      select pg_drop_replication_slot('peek');
select pg_create_logical_replication_slot('peek', 'test_decoding');

--    The Go recorder will use the pgoutput format, which needs a publication:
--    the list of tables whose changes get sent.
create publication undo_pub for table customers, orders;
