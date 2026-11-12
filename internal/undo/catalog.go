package undo

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// table is what undo needs to know about a table as it is now.
type table struct {
	relid   uint32
	display string   // schema.table, for people
	ident   string   // schema.table, quoted for SQL
	key     []string // primary key columns, in index order
	columns []string // columns undo may compare and write, in table order
	has     map[string]bool
	// identityAlways is set when a column is GENERATED ALWAYS AS IDENTITY,
	// which an INSERT may only fill with OVERRIDING SYSTEM VALUE.
	identityAlways bool
	rls            bool     // row-level security would hide rows from undo
	hooks          []string // user triggers and rules that would fire on undo's writes
	referencedBy   []foreignKey
	parents        []uint32 // tables this one has foreign keys to
}

// foreignKey is a foreign key in another table that points at this one.
type foreignKey struct {
	child        uint32
	childIdent   string // quoted, for SQL
	childDisplay string
	childRLS     bool // row-level security hides some of the child's rows from undo
	childCols    []string
	parentCols   []string
	onDelete     byte // 'a' no action, 'r' restrict, 'c' cascade, 'n' set null, 'd' set default
	onUpdate     byte
}

// changesOthers reports whether a foreign key action changes the referencing
// rows, instead of refusing the change.
func changesOthers(action byte) bool { return action == 'c' || action == 'n' || action == 'd' }

// loadTables reads the current definition of each table. Only tables that
// agent-undo tags are loaded; a receipt naming anything else (a system
// catalog, agent-undo's own tables, a dropped table) gets its rows refused.
func loadTables(ctx context.Context, tx pgx.Tx, relids []uint32) (map[uint32]*table, error) {
	tables := map[uint32]*table{}
	rows, err := tx.Query(ctx, `
		select c.oid, n.nspname, c.relname, row_security_active(c.oid)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.oid = any($1) and c.relkind in ('r', 'p')
		  and n.nspname not in ('information_schema', 'agent_undo') and n.nspname not like 'pg\_%'
		  and exists (select from pg_trigger g
		              where g.tgrelid = c.oid and g.tgfoid = to_regproc('agent_undo.tag_statement'))`, relids)
	if err != nil {
		return nil, err
	}
	var relid uint32
	var schema, name string
	var rls bool
	_, err = pgx.ForEachRow(rows, []any{&relid, &schema, &name, &rls}, func() error {
		tables[relid] = &table{
			relid: relid, display: schema + "." + name, ident: pgx.Identifier{schema, name}.Sanitize(),
			rls: rls, has: map[string]bool{},
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}

	// Primary keys.
	rows, err = tx.Query(ctx, `
		select i.indrelid, a.attname::text
		from pg_index i
		cross join lateral unnest(i.indkey::int2[]) with ordinality k(attnum, pos)
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
		where i.indrelid = any($1) and i.indisprimary
		order by i.indrelid, k.pos`, relids)
	if err != nil {
		return nil, err
	}
	var column string
	_, err = pgx.ForEachRow(rows, []any{&relid, &column}, func() error {
		if t := tables[relid]; t != nil {
			t.key = append(t.key, column)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read primary keys: %w", err)
	}

	// Columns. Generated columns are computed by Postgres, so undo leaves them out.
	rows, err = tx.Query(ctx, `
		select attrelid, attname::text, attidentity = 'a'
		from pg_attribute
		where attrelid = any($1) and attnum > 0 and not attisdropped and attgenerated = ''
		order by attrelid, attnum`, relids)
	if err != nil {
		return nil, err
	}
	var identityAlways bool
	_, err = pgx.ForEachRow(rows, []any{&relid, &column, &identityAlways}, func() error {
		if t := tables[relid]; t != nil {
			t.columns = append(t.columns, column)
			t.has[column] = true
			t.identityAlways = t.identityAlways || identityAlways
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	// Triggers and rules that would fire on undo's own writes. Internal
	// triggers enforce foreign keys and are fine; agent-undo's tag trigger
	// does nothing for undo's role.
	rows, err = tx.Query(ctx, `
		select tgrelid, 'trigger ' || tgname from pg_trigger
		where tgrelid = any($1) and not tgisinternal and tgenabled in ('O', 'A')
		  and tgfoid is distinct from to_regproc('agent_undo.tag_statement')
		union all
		select ev_class, 'rule ' || rulename from pg_rewrite
		where ev_class = any($1) and rulename <> '_RETURN'`, relids)
	if err != nil {
		return nil, err
	}
	var hook string
	_, err = pgx.ForEachRow(rows, []any{&relid, &hook}, func() error {
		if t := tables[relid]; t != nil {
			t.hooks = append(t.hooks, hook)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read triggers: %w", err)
	}

	return tables, loadForeignKeys(ctx, tx, tables, relids)
}

// loadForeignKeys reads the foreign keys that point at the tables, and which
// of the tables refer to which.
func loadForeignKeys(ctx context.Context, tx pgx.Tx, tables map[uint32]*table, relids []uint32) error {
	rows, err := tx.Query(ctx, `
		select f.confrelid, f.conrelid, n.nspname, c.relname, row_security_active(f.conrelid),
			array(select a.attname::text from unnest(f.conkey) with ordinality k(attnum, pos)
			      join pg_attribute a on a.attrelid = f.conrelid and a.attnum = k.attnum order by k.pos),
			array(select a.attname::text from unnest(f.confkey) with ordinality k(attnum, pos)
			      join pg_attribute a on a.attrelid = f.confrelid and a.attnum = k.attnum order by k.pos),
			f.confdeltype::text, f.confupdtype::text
		from pg_constraint f
		join pg_class c on c.oid = f.conrelid
		join pg_namespace n on n.oid = c.relnamespace
		where f.contype = 'f' and f.confrelid = any($1)`, relids)
	if err != nil {
		return err
	}
	var parent, child uint32
	var schema, name, onDelete, onUpdate string
	var childRLS bool
	var childCols, parentCols []string
	_, err = pgx.ForEachRow(rows, []any{&parent, &child, &schema, &name, &childRLS, &childCols, &parentCols, &onDelete, &onUpdate}, func() error {
		t := tables[parent]
		if t == nil {
			return nil
		}
		t.referencedBy = append(t.referencedBy, foreignKey{
			child: child, childIdent: pgx.Identifier{schema, name}.Sanitize(), childDisplay: schema + "." + name,
			childRLS: childRLS, childCols: slices.Clone(childCols), parentCols: slices.Clone(parentCols),
			onDelete: onDelete[0], onUpdate: onUpdate[0],
		})
		if c := tables[child]; c != nil && child != parent {
			c.parents = append(c.parents, parent)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read foreign keys: %w", err)
	}
	return nil
}

// rank orders tables so that every table comes after the tables it refers
// to: parents get lower ranks. Tables in a cycle get the same rank.
func rank(tables map[uint32]*table) map[uint32]int {
	ranks := map[uint32]int{}
	var visit func(id uint32, path map[uint32]bool) int
	visit = func(id uint32, path map[uint32]bool) int {
		if r, ok := ranks[id]; ok {
			return r
		}
		if path[id] {
			return 0 // a cycle; the write retries sort it out
		}
		path[id] = true
		r := 0
		for _, p := range tables[id].parents {
			r = max(r, visit(p, path)+1)
		}
		delete(path, id)
		ranks[id] = r
		return r
	}
	for id := range tables {
		visit(id, map[uint32]bool{})
	}
	return ranks
}
