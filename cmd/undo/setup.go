package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/setup"
)

func runInit(ctx context.Context, a *app, args []string) error {
	fs := a.flags("init")
	dbURL, receipts := dbFlag(fs), receiptsFlag(fs)
	var s setup.Settings
	fs.StringVar(&s.AgentRole, "agent-role", "agent_bot", "the role AI agents log in as")
	fs.StringVar(&s.Slot, "slot", "undo_slot", "replication slot name")
	fs.StringVar(&s.Publication, "publication", "undo_pub", "publication name")
	newEpoch := fs.Bool("new-epoch", false, "start recording again after the slot was lost, marking the gap")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	conn, cfg, err := a.connect(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	res, err := setup.Init(ctx, cfg, *receipts, s, *newEpoch)
	if err != nil {
		return err
	}
	if res.CreatedSlot {
		a.log.Info("created replication slot", "slot", s.Slot)
	}
	if res.Gap != nil {
		a.log.Warn("recording restarts with a gap; undo will refuse runs that started before it",
			"from", res.Gap.From, "to", res.Gap.To)
	}
	fmt.Fprintf(a.stdout, "Tagged %d table(s); agent writes come from role %s.\n", len(res.Tables), s.AgentRole)
	for _, t := range res.Tables {
		fmt.Fprintln(a.stdout, printable("  "+t))
	}
	fmt.Fprintf(a.stdout, "Receipts go to %s. Start the recorder with: undo record\n\n", *receipts)
	return report(ctx, a, conn)
}

func runCheck(ctx context.Context, a *app, args []string) error {
	fs := a.flags("check")
	dbURL := dbFlag(fs)
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	conn, _, err := a.connect(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return report(ctx, a, conn)
}

// report prints every finding of setup.Check and fails if any is a problem.
func report(ctx context.Context, a *app, conn *pgx.Conn) error {
	settings, findings, err := setup.Check(ctx, conn)
	if err != nil {
		return err
	}
	for _, f := range findings {
		fmt.Fprintln(a.stdout, printable(f.String()))
	}
	if setup.HasProblem(findings) {
		return errors.New("recording is not safe until the errors above are fixed")
	}
	if len(findings) == 0 {
		fmt.Fprintf(a.stdout, "OK: every table is tagged, so all of %s's writes are recorded.\n", settings.AgentRole)
	}
	return nil
}

func runTeardown(ctx context.Context, a *app, args []string) error {
	fs := a.flags("teardown")
	dbURL := dbFlag(fs)
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	conn, _, err := a.connect(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	s, err := setup.Teardown(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Removed agent-undo and replication slot %s. Receipts were kept.\n", s.Slot)
	return nil
}
