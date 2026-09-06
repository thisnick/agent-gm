package main

import (
	"context"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/store"
)

// R-16. Migration 0004 was the first thing ever to write
// `server_meta.pending_reprocess`, and nothing read it. Section 4.3's clause
// is "the process runs that task once after startup and clears the key", and
// only the first half existed: the key stayed set for ever on every upgraded
// database while the rows it was set for kept their raw participant IDs.
//
// A migration that hands work to a task nothing runs has not deferred the
// work; it has dropped it. This asserts the whole clause through the
// binary's own startup, not through a helper that resembles it.
//
// Plant: delete the core.RunPendingReprocess call in serve.go and this fails
// at "pending_reprocess is still set". Planted 2026-09-07.
func TestServeRunsThePendingReprocessTaskAndClearsTheKey(t *testing.T) {
	dir := t.TempDir()
	serveEnv(t, dir)
	ctx := context.Background()

	// A first process: pair, so there is an account for the task to sweep.
	first, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("buildServer returned exit %d", code)
	}
	backend, err := first.Deps.NewBackend()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Sup.Pair(ctx, backend, fixtureCookies(), 0, func(string) {}); err != nil {
		t.Fatalf("pairing: %v", err)
	}
	// The state an upgraded database is in: the migration ran and left work.
	if err := first.Store.SetMeta(ctx, core.PendingReprocessKey,
		core.TaskReconcileParticipants); err != nil {
		t.Fatal(err)
	}
	first.Sup.StopAll(ctx)
	first.Close()

	// The next start must run the task and clear the key.
	second, code := buildServer(ctx, "127.0.0.1:0")
	if code != exitOK {
		t.Fatalf("the second buildServer returned exit %d", code)
	}
	defer second.Close()
	defer second.Sup.StopAll(context.WithoutCancel(ctx))

	value, ok, err := second.Store.Meta(ctx, core.PendingReprocessKey)
	if err != nil {
		t.Fatal(err)
	}
	if ok && value != "" {
		t.Fatalf("pending_reprocess is still set to %q after a start; section 4.3 says "+
			"the process runs the task once and clears the key", value)
	}
}

// The key is cleared only when the work was DONE. A sweep that failed leaves
// it set, so the next start tries again rather than declaring the work
// finished because it was attempted.
func TestAFailedReprocessLeavesTheKeySet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.SetMeta(ctx, core.PendingReprocessKey, core.TaskReconcileParticipants); err != nil {
		t.Fatal(err)
	}
	failing := sweeperFunc(func(context.Context, string, time.Time) error {
		return context.DeadlineExceeded
	})
	task, ran, err := core.RunPendingReprocess(ctx, st, failing, []string{"acct_x"})
	if err == nil {
		t.Fatal("a failed sweep was reported as success")
	}
	if ran {
		t.Error("a failed sweep was reported as having run")
	}
	if task != core.TaskReconcileParticipants {
		t.Errorf("task = %q", task)
	}
	value, ok, err := st.Meta(ctx, core.PendingReprocessKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != core.TaskReconcileParticipants {
		t.Errorf("the key was cleared after a failure: %q (present=%v)", value, ok)
	}
}

// A key naming a task this build does not know is LEFT ALONE, not cleared: a
// newer binary wrote it, this one cannot do the work, and clearing it would
// lose the request silently.
func TestAnUnknownReprocessTaskIsReportedNotCleared(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.SetMeta(ctx, core.PendingReprocessKey, "a_task_from_a_newer_build"); err != nil {
		t.Fatal(err)
	}
	noop := sweeperFunc(func(context.Context, string, time.Time) error { return nil })
	if _, ran, err := core.RunPendingReprocess(ctx, st, noop, nil); err == nil || ran {
		t.Fatal("an unknown task was silently accepted")
	}
	value, ok, err := st.Meta(ctx, core.PendingReprocessKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "a_task_from_a_newer_build" {
		t.Errorf("an unknown task's key was cleared: %q (present=%v)", value, ok)
	}
}

// No key means no work and no error: the ordinary start.
func TestNoPendingReprocessIsNotAnError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, clock.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	task, ran, err := core.RunPendingReprocess(ctx, st, nil, nil)
	if err != nil || ran || task != "" {
		t.Fatalf("a clean database reported %q / ran=%v / %v", task, ran, err)
	}
}

type sweeperFunc func(context.Context, string, time.Time) error

func (f sweeperFunc) Sweep(ctx context.Context, id string, since time.Time) error {
	return f(ctx, id, since)
}
