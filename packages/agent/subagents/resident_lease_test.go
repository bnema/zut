package subagents

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestResidentJournalLeaseExcludesOtherProcess(t *testing.T) {
	if os.Getenv("ZUT_RESIDENT_LEASE_HELPER") == "1" {
		journal, err := OpenResidentJournal(os.Getenv("ZUT_RESIDENT_LEASE_ROOT"), "child")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "ready")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResidentJournalLeaseExcludesOtherProcess$")
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	cmd.Env = append(os.Environ(), "ZUT_RESIDENT_LEASE_HELPER=1", "ZUT_RESIDENT_LEASE_ROOT="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if ready != "ready\n" {
		t.Fatalf("helper readiness = %q", ready)
	}
	if _, err := OpenResidentJournal(root, "child"); !errors.Is(err, ErrResidentLeaseBusy) {
		t.Fatalf("OpenResidentJournal while other process owns lease = %v, want ErrResidentLeaseBusy", err)
	}
	if _, err := fmt.Fprintln(stdin); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenResidentJournal(root, "child")
	if err != nil {
		t.Fatalf("OpenResidentJournal after owner exit: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResidentJournalLeaseExcludesSecondOwner(t *testing.T) {
	root := t.TempDir()
	first, err := OpenResidentJournal(root, "child")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := OpenResidentJournal(root, "child"); !errors.Is(err, ErrResidentLeaseBusy) {
		t.Fatalf("second OpenResidentJournal error = %v, want ErrResidentLeaseBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenResidentJournal(root, "child")
	if err != nil {
		t.Fatalf("OpenResidentJournal after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
