package setup

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Severity says how serious a finding is.
type Severity int

const (
	Warning Severity = iota // worth fixing; recording and undo still work
	Problem                 // some agent changes could go unrecorded or be undone unsafely
)

func (s Severity) String() string {
	if s == Problem {
		return "ERROR"
	}
	return "WARN"
}

// Finding is one problem Check found.
type Finding struct {
	Severity Severity
	Message  string
}

func (f Finding) String() string { return f.Severity.String() + "  " + f.Message }

// HasProblem reports whether any finding is a Problem.
func HasProblem(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == Problem {
			return true
		}
	}
	return false
}

// agentCan is true when the agent role, or any role it can act as (SET ROLE),
// has one of the privileges on table c. $1 is the agent role.
const agentCan = `exists (select from pg_roles r where pg_has_role($1, r.oid, 'MEMBER')
	and (has_table_privilege(r.oid, c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE')
	     or has_any_column_privilege(r.oid, c.oid, 'INSERT, UPDATE')))`

// Check inspects the database and returns every problem it finds, so an
// operator sees them all at once. It also returns the settings stored by
// undo init. A non-nil error means the checks themselves could not run.
func Check(ctx context.Context, conn *pgx.Conn) (Settings, []Finding, error) {
	c := &checker{conn: conn}
	if err := c.server(ctx); err != nil {
		return Settings{}, nil, err
	}
	var err error
	c.s, err = ReadSettings(ctx, conn)
	if errors.Is(err, ErrNotInstalled) {
		c.add(Problem, "%s", err)
		return Settings{}, c.findings, nil
	}
	if err != nil {
		return Settings{}, nil, err
	}
	for _, step := range []func(context.Context) error{c.agentRole, c.tables, c.unrecordable, c.publication, c.slot} {
		if err := step(ctx); err != nil {
			return Settings{}, nil, err
		}
	}
	return c.s, c.findings, nil
}

type checker struct {
	conn     *pgx.Conn
	s        Settings
	version  int
	findings []Finding
}

func (c *checker) add(sev Severity, format string, args ...any) {
	c.findings = append(c.findings, Finding{sev, fmt.Sprintf(format, args...)})
}

func (c *checker) server(ctx context.Context) error {
	var walLevel, encoding, keepSize string
	err := c.conn.QueryRow(ctx, `select current_setting('server_version_num')::int,
		current_setting('wal_level'), current_setting('server_encoding'),
		current_setting('max_slot_wal_keep_size')`).Scan(&c.version, &walLevel, &encoding, &keepSize)
	if err != nil {
		return fmt.Errorf("read server settings: %w", err)
	}
	if c.version < 140000 {
		c.add(Problem, "Postgres %d is too old: agent-undo needs version 14 or later", c.version/10000)
	}
	if walLevel != "logical" {
		c.add(Problem, "wal_level is %s: set wal_level = logical and restart Postgres", walLevel)
	}
	if encoding != "UTF8" {
		c.add(Problem, "server_encoding is %s: receipts need a UTF8 database", encoding)
	}
	if keepSize == "-1" {
		c.add(Warning, "max_slot_wal_keep_size is unlimited: if the recorder stops for long, WAL can fill the disk")
	}
	return nil
}

// agentRole checks what the agent role can do besides writing rows.
func (c *checker) agentRole(ctx context.Context) error {
	var exists bool
	if err := c.conn.QueryRow(ctx, `select exists (select from pg_roles where rolname = $1)`, c.s.AgentRole).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		c.add(Problem, "the agent role %q does not exist", c.s.AgentRole)
		return nil
	}

	rows, err := c.conn.Query(ctx, `
		select rolname, rolsuper from pg_roles
		where pg_has_role($1, oid, 'MEMBER') and (rolsuper or rolreplication)`, c.s.AgentRole)
	if err != nil {
		return err
	}
	var role string
	var super bool
	_, err = pgx.ForEachRow(rows, []any{&role, &super}, func() error {
		what := "has REPLICATION, so it could read or move the recorder's slot"
		if super {
			what = "is a superuser, so it could switch the trigger off"
		}
		if role == c.s.AgentRole {
			c.add(Problem, "the agent role %s", what)
		} else {
			c.add(Problem, "the agent role can act as %s, which %s", role, what)
		}
		return nil
	})
	if err != nil {
		return err
	}

	rows, err = c.conn.Query(ctx, `
		select n.nspname from pg_namespace n
		where n.nspname not like 'pg\_%' and n.nspname <> 'information_schema'
		  and exists (select from pg_roles r where pg_has_role($1, r.oid, 'MEMBER')
		              and has_schema_privilege(r.oid, n.oid, 'CREATE'))
		order by 1`, c.s.AgentRole)
	if err != nil {
		return err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		c.add(Problem, "the agent role can create tables in schema %s; it would own them and could switch their trigger off: revoke CREATE", schema)
	}

	var canWriteUndo bool
	err = c.conn.QueryRow(ctx, `
		select exists (select from pg_class c join pg_namespace n on n.oid = c.relnamespace
		               where n.nspname = 'agent_undo' and c.relkind = 'r' and `+agentCan+`)`,
		c.s.AgentRole).Scan(&canWriteUndo)
	if err != nil {
		return err
	}
	if canWriteUndo {
		c.add(Problem, "the agent role can write agent-undo's own tables, so it could forge tags or unseal runs")
	}
	return nil
}

// tables checks that every table is tagged, logs whole rows and is published.
func (c *checker) tables(ctx context.Context) error {
	rows, err := c.conn.Query(ctx, `
		select n.nspname || '.' || c.relname, c.relkind::text, c.relreplident = 'f',
			exists (select from pg_index i where i.indrelid = c.oid and i.indisprimary),
			exists (select from pg_trigger g
			        where g.tgrelid = c.oid and g.tgname = 'agent_undo_tag'
			          and g.tgfoid = to_regproc('agent_undo.tag_statement')
			          and g.tgenabled = 'A' and g.tgtype = 62),
			-- Published itself, or through a partitioned table above it.
			-- (pg_partition_ancestors returns nothing for a plain table.)
			exists (select from pg_publication_rel pr
			        join pg_publication p on p.oid = pr.prpubid
			        where p.pubname = $2
			          and (pr.prrelid = c.oid
			               or pr.prrelid in (select relid from pg_partition_ancestors(c.oid)))),
			pg_has_role($1, c.relowner, 'MEMBER')
			  or exists (select from pg_roles r where pg_has_role($1, r.oid, 'MEMBER')
			             and has_table_privilege(r.oid, c.oid, 'TRIGGER')),
			`+agentCan+`
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'p') and c.relpersistence = 'p'
		  and n.nspname not in ('information_schema', 'agent_undo') and n.nspname not like 'pg\_%'
		order by 1`, c.s.AgentRole, c.s.Publication)
	if err != nil {
		return fmt.Errorf("check tables: %w", err)
	}
	var name, kind string
	var fullIdentity, hasKey, tagged, published, agentControls, agentWrites bool
	_, err = pgx.ForEachRow(rows, []any{&name, &kind, &fullIdentity, &hasKey, &tagged, &published, &agentControls, &agentWrites}, func() error {
		if !tagged {
			c.add(Problem, "%s is not tagged, so the agent's changes to it would not be recorded: run undo init", name)
		}
		if kind == "r" && !fullIdentity {
			c.add(Problem, "%s does not log whole old rows (REPLICA IDENTITY FULL): run undo init", name)
		}
		if !published {
			c.add(Problem, "%s is not in publication %s: run undo init", name, c.s.Publication)
		}
		if agentControls {
			c.add(Problem, "the agent role owns %s or holds TRIGGER on it, so it could switch the trigger off", name)
		}
		if agentWrites && kind == "r" && !hasKey {
			c.add(Warning, "%s has no primary key, so undo will refuse the agent's changes to it", name)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check tables: %w", err)
	}
	return nil
}

// unrecordable reports tables the agent can write whose changes never reach
// the WAL stream: unlogged tables and foreign tables.
func (c *checker) unrecordable(ctx context.Context) error {
	rows, err := c.conn.Query(ctx, `
		select n.nspname || '.' || c.relname, c.relkind = 'f'
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where (c.relkind = 'f' or (c.relkind in ('r', 'p') and c.relpersistence = 'u')) and `+agentCan+`
		order by 1`, c.s.AgentRole)
	if err != nil {
		return err
	}
	var name string
	var foreign bool
	_, err = pgx.ForEachRow(rows, []any{&name, &foreign}, func() error {
		what := "an unlogged table"
		if foreign {
			what = "a foreign table"
		}
		c.add(Problem, "the agent role can write %s, %s whose changes can't be recorded", name, what)
		return nil
	})
	return err
}

func (c *checker) publication(ctx context.Context) error {
	var allOps, viaRoot, hasTag bool
	err := c.conn.QueryRow(ctx, `
		select pubinsert and pubupdate and pubdelete and pubtruncate, pubviaroot,
			exists (select from pg_publication_rel pr
			        where pr.prpubid = p.oid and pr.prrelid = to_regclass('agent_undo.tag'))
		from pg_publication p where p.pubname = $1`, c.s.Publication).Scan(&allOps, &viaRoot, &hasTag)
	if errors.Is(err, pgx.ErrNoRows) {
		c.add(Problem, "publication %s does not exist: run undo init", c.s.Publication)
		return nil
	}
	if err != nil {
		return fmt.Errorf("check the publication: %w", err)
	}
	if !allOps {
		c.add(Problem, "publication %s does not publish every kind of change: run undo init", c.s.Publication)
	}
	if viaRoot {
		c.add(Problem, "publication %s uses publish_via_partition_root, which agent-undo does not support", c.s.Publication)
	}
	if !hasTag {
		c.add(Problem, "publication %s does not include agent_undo.tag: run undo init", c.s.Publication)
	}
	if c.version >= 150000 { // row filters and column lists arrived in Postgres 15
		var filtered bool
		err := c.conn.QueryRow(ctx, `
			select exists (select from pg_publication_rel r join pg_publication p on p.oid = r.prpubid
			               where p.pubname = $1 and (r.prqual is not null or r.prattrs is not null))`,
			c.s.Publication).Scan(&filtered)
		if err != nil {
			return fmt.Errorf("check the publication: %w", err)
		}
		if filtered {
			c.add(Problem, "publication %s filters rows or columns, so some agent changes would be missing", c.s.Publication)
		}
	}
	return nil
}

func (c *checker) slot(ctx context.Context) error {
	slot, err := ReadSlot(ctx, c.conn, c.s.Slot)
	if err != nil {
		return err
	}
	var database string
	if err := c.conn.QueryRow(ctx, `select current_database()`).Scan(&database); err != nil {
		return err
	}
	switch {
	case !slot.Exists:
		c.add(Problem, "replication slot %s does not exist, so agent writes are blocked: run undo init", c.s.Slot)
		return nil
	case slot.Plugin != "pgoutput" || slot.Database != database:
		c.add(Problem, "replication slot %s belongs to database %q with plugin %q, not %q with pgoutput",
			c.s.Slot, slot.Database, slot.Plugin, database)
	case slot.WALStatus == "lost":
		c.add(Problem, "replication slot %s was invalidated, so agent writes are blocked and changes are missing: run undo init --new-epoch", c.s.Slot)
	case slot.Confirmed == 0:
		c.add(Problem, "replication slot %s is still being created, so agent writes are blocked", c.s.Slot)
	case slot.WALStatus == "unreserved":
		c.add(Warning, "replication slot %s is about to lose WAL it still needs: is undo record running?", c.s.Slot)
	}

	var behind int64
	err = c.conn.QueryRow(ctx, `
		select coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn), 0)::bigint
		from pg_replication_slots where slot_name = $1`, c.s.Slot).Scan(&behind)
	if err != nil {
		return err
	}
	if behind > 1<<30 {
		c.add(Warning, "the recorder is %d MB behind: is undo record running?", behind>>20)
	}

	rows, err := c.conn.Query(ctx, `select slot_name from pg_replication_slots where not active and slot_name <> $1 order by 1`, c.s.Slot)
	if err != nil {
		return err
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range others {
		c.add(Warning, "inactive replication slot %s is holding WAL: drop it if nothing uses it", name)
	}
	return nil
}
