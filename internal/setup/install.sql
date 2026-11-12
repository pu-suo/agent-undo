-- agent-undo's objects in the database. undo init runs this as an admin in one
-- transaction, then tracks tables. Running it again is safe.

create schema if not exists agent_undo;
revoke all on schema agent_undo from public;

-- Who the agent is, which slot records it and which publication it reads.
create table if not exists agent_undo.settings (
    id          boolean primary key default true check (id),
    agent_role  name not null,
    slot_name   name not null,
    publication name not null
);

-- The latest run ID of each agent connection. Only the tag trigger writes
-- here. The recorder reads these rows from the WAL: a change that follows a
-- tag row in the same transaction belongs to that run.
create table if not exists agent_undo.tag (
    pid integer primary key,
    run text    not null
);

-- One row per run: where in the WAL it started, and whether undo has sealed
-- it. Every agent statement share-locks its run's row, so sealing (an
-- UPDATE) waits for the run's open transactions, and a transaction that
-- began before the seal fails instead of writing after it.
create table if not exists agent_undo.run (
    run       text   primary key,
    first_lsn pg_lsn not null,
    sealed    boolean not null default false
);

revoke all on all tables in schema agent_undo from public;

-- Runs before every statement on a tracked table. For the agent's statements
-- it checks the run ID and that recording works, then writes a tag row, which
-- lands in the WAL before the statement's own changes.
create or replace function agent_undo.tag_statement() returns trigger
language plpgsql
security definer
set search_path = pg_catalog, pg_temp
as $$
declare
    s      agent_undo.settings;
    v_run  text := current_setting('application_name');
    sealed boolean;
begin
    select * into s from agent_undo.settings;
    if not found then
        raise exception 'agent-undo is not set up: agent_undo.settings is empty';
    end if;
    if session_user <> s.agent_role then
        return null; -- a person's write: nothing to tag
    end if;
    if v_run !~ '^run-[a-z0-9][a-z0-9._-]{0,58}$' then
        raise exception 'agent-undo: application_name % is not a valid run ID', quote_literal(v_run)
            using hint = 'Connect with application_name set to run-<id>, using lowercase letters, digits, ".", "_" or "-".';
    end if;
    -- A slot still being created has no confirmed position yet, and it would
    -- miss this transaction.
    if not exists (select from pg_replication_slots
                   where slot_name = s.slot_name and database = current_database()
                     and plugin = 'pgoutput' and confirmed_flush_lsn is not null
                     and coalesce(wal_status, '') <> 'lost') then
        raise exception 'agent-undo: replication slot % is missing, lost or not ready, so agent writes are blocked', s.slot_name
            using hint = 'Run undo init to set up recording again.';
    end if;

    insert into agent_undo.run (run, first_lsn) values (v_run, pg_current_wal_insert_lsn())
        on conflict (run) do nothing;
    select r.sealed into sealed from agent_undo.run r where r.run = v_run for share;
    if sealed then
        raise exception 'agent-undo: run % has been sealed for undo, so its writes are blocked', v_run;
    end if;

    insert into agent_undo.tag as t (pid, run) values (pg_backend_pid(), v_run)
        on conflict (pid) do update set run = excluded.run;
    return null;
end
$$;

revoke all on function agent_undo.tag_statement() from public;
