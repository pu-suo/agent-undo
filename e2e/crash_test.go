package e2e

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/pu-suo/agent-undo/internal/capture"
	"github.com/pu-suo/agent-undo/internal/db"
)

// TestMain lets the test binary double as a recorder process, so the crash
// test can kill a real process at any instruction.
func TestMain(m *testing.M) {
	if os.Getenv("AGENT_UNDO_CHILD") == "recorder" {
		cfg, err := db.Config(os.Getenv("AGENT_UNDO_CHILD_DB"))
		if err != nil {
			os.Exit(1)
		}
		rec := &capture.Recorder{Config: cfg, Receipts: os.Getenv("AGENT_UNDO_CHILD_RECEIPTS"),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if err := rec.Record(context.Background()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The recorder is killed with SIGKILL over and over while an agent commits
// one row per transaction. Afterwards every row must be in the receipt
// exactly once: nothing lost, nothing repeated.
func TestCrashedRecorderLosesNothingAndRepeatsNothing(t *testing.T) {
	d := newDB(t, `create table items (id int primary key); grant insert on items to undo_test_agent;`)
	const rows, batch = 600, 10

	agentDone := make(chan error, 1)
	go func() {
		// Small batches with pauses, so the kills land while the agent writes.
		for first := 1; first <= rows; first += batch {
			statements := make([]string, batch)
			for i := range statements {
				statements[i] = "insert into items values (" + strconv.Itoa(first+i) + ")"
			}
			if err := d.agent(t, "run-crash", statements...); err != nil {
				agentDone <- err
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		agentDone <- nil
	}()

	kills := 0
	for agentRunning := true; agentRunning; {
		child := d.startChildRecorder(t)
		time.Sleep(time.Duration(50+rand.IntN(250)) * time.Millisecond)
		_ = child.Process.Kill() // SIGKILL: no chance to clean up
		_ = child.Wait()         // reports the kill, which is expected
		kills++
		select {
		case err := <-agentDone:
			if err != nil {
				t.Fatal(err)
			}
			agentRunning = false
		default:
		}
	}

	child := d.startChildRecorder(t) // a final, undisturbed run
	d.waitCaughtUp(t)
	_ = child.Process.Kill()
	_ = child.Wait()

	seen := map[string]int{}
	for _, txn := range d.read(t, "run-crash") {
		for _, c := range txn.Changes {
			seen[*c.New["id"]]++
		}
	}
	for i := 1; i <= rows; i++ {
		if n := seen[strconv.Itoa(i)]; n != 1 {
			t.Errorf("row %d is in the receipt %d times, want once", i, n)
		}
	}
	if kills < 5 {
		t.Errorf("the recorder was killed only %d times while the agent wrote; the test proves little", kills)
	}
	t.Logf("killed the recorder %d times; %d rows recorded once each", kills, len(seen))
}

// startChildRecorder runs a recorder in a separate process.
func (d *testDB) startChildRecorder(t *testing.T) *exec.Cmd {
	t.Helper()
	u, err := url.Parse(adminURL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + d.name
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "AGENT_UNDO_CHILD=recorder",
		"AGENT_UNDO_CHILD_DB="+u.String(), "AGENT_UNDO_CHILD_RECEIPTS="+d.receipts)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}
