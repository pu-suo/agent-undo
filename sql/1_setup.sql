-- 1_setup.sql: run once, as the admin user (postgres):
--   docker compose exec db psql -U postgres -d shop -f /sql/1_setup.sql
--
-- Builds a tiny shop and gives the "agent" its own login. Recording itself
-- (the tag trigger, the publication and the replication slot) is set up
-- afterwards by `undo init`.

-- A tiny shop to experiment on: 20 customers, 40 orders.
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

-- The agent gets its own login, so its writes can be told apart. It owns
-- nothing and may only read and change rows: no TRUNCATE, no DDL.
create role agent_bot login;
grant select, insert, update, delete on customers, orders to agent_bot;
