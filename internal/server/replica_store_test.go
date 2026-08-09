package server

import (
	"testing"
	"time"
)

func TestEnqueueReplicaIsIdempotent(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()

	for i := 0; i < 3; i++ {
		if err := st.EnqueueReplica(ReplicaKindRun, "run-1", t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("EnqueueReplica: %v", err)
		}
	}
	items, err := st.ReplicaItems(0)
	if err != nil {
		t.Fatalf("ReplicaItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want one row after three enqueues, got %d", len(items))
	}
	if items[0].Status != ReplicaPending {
		t.Fatalf("status = %q, want pending", items[0].Status)
	}
	// The queue time is the age of the backlog. Re-queueing something that has not been
	// sent yet must not reset it, or "waiting two hours" would never become true.
	if !items[0].QueuedAt.Equal(t0.Truncate(time.Millisecond)) {
		t.Fatalf("queued_at moved on re-enqueue: got %v, want %v", items[0].QueuedAt, t0)
	}
}

func TestEnqueueReplicaRequeuesFinishedItem(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRepo, "files-web01", t0)
	if err := st.MarkReplicaDone(ReplicaKindRepo, "files-web01", t0, 4096); err != nil {
		t.Fatalf("MarkReplicaDone: %v", err)
	}

	later := t0.Add(time.Hour)
	mustEnqueue(t, st, ReplicaKindRepo, "files-web01", later)

	it := mustItem(t, st, ReplicaKindRepo, "files-web01")
	if it.Status != ReplicaPending {
		t.Fatalf("status = %q, want pending after re-enqueue", it.Status)
	}
	if !it.QueuedAt.Equal(later.Truncate(time.Millisecond)) {
		t.Fatalf("queued_at = %v, want the new time %v", it.QueuedAt, later)
	}
}

func TestEnqueueReplicaLeavesTransferInFlightAlone(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRun, "run-7", t0)
	if err := st.MarkReplicaSyncing(ReplicaKindRun, "run-7", t0); err != nil {
		t.Fatalf("MarkReplicaSyncing: %v", err)
	}
	mustEnqueue(t, st, ReplicaKindRun, "run-7", t0.Add(time.Minute))

	if it := mustItem(t, st, ReplicaKindRun, "run-7"); it.Status != ReplicaSyncing {
		t.Fatalf("status = %q, want the in-flight transfer left as syncing", it.Status)
	}
}

func TestReplicaFailureBacksOffAndNeverDropsTheItem(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRun, "run-3", t0)

	for i := 1; i <= 3; i++ {
		if err := st.MarkReplicaFailed(ReplicaKindRun, "run-3", t0, "connection refused"); err != nil {
			t.Fatalf("MarkReplicaFailed: %v", err)
		}
		it := mustItem(t, st, ReplicaKindRun, "run-3")
		if it.Attempts != i {
			t.Fatalf("attempts = %d, want %d", it.Attempts, i)
		}
		if it.Status != ReplicaFailed {
			t.Fatalf("status = %q, want failed", it.Status)
		}
		want := t0.Add(replicaBackoff(i)).Truncate(time.Millisecond)
		if !it.NextTryAt.Equal(want) {
			t.Fatalf("attempt %d: next_try_at = %v, want %v", i, it.NextTryAt, want)
		}
	}
	// The whole point of the queue: a link that comes back finds the work still there.
	due, err := st.DueReplicaItems(t0.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("DueReplicaItems: %v", err)
	}
	if len(due) != 1 || due[0].Key != "run-3" {
		t.Fatalf("a failed item must stay due once its backoff has passed, got %+v", due)
	}
}

func TestReplicaBackoffIsCapped(t *testing.T) {
	if got := replicaBackoff(1); got != replicaBackoffBase {
		t.Fatalf("first retry = %v, want %v", got, replicaBackoffBase)
	}
	if got := replicaBackoff(2); got != 2*replicaBackoffBase {
		t.Fatalf("second retry = %v, want %v", got, 2*replicaBackoffBase)
	}
	if got := replicaBackoff(50); got != replicaBackoffMax {
		t.Fatalf("far-out retry = %v, want the cap %v", got, replicaBackoffMax)
	}
}

func TestDueReplicaItemsSkipsBackoffAndDone(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRun, "run-1", t0)
	mustEnqueue(t, st, ReplicaKindRun, "run-2", t0)
	if err := st.MarkReplicaDone(ReplicaKindRun, "run-1", t0, 1); err != nil {
		t.Fatalf("MarkReplicaDone: %v", err)
	}
	if err := st.MarkReplicaFailed(ReplicaKindRun, "run-2", t0, "boom"); err != nil {
		t.Fatalf("MarkReplicaFailed: %v", err)
	}

	if due, err := st.DueReplicaItems(t0, 0); err != nil || len(due) != 0 {
		t.Fatalf("nothing should be due while run-2 is in backoff: %v, %v", due, err)
	}
	due, err := st.DueReplicaItems(t0.Add(replicaBackoffBase), 0)
	if err != nil {
		t.Fatalf("DueReplicaItems: %v", err)
	}
	if len(due) != 1 || due[0].Key != "run-2" {
		t.Fatalf("want only run-2 due, got %+v", due)
	}
}

func TestRetryReplicaFailedSkipsBackoff(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRun, "run-1", t0)
	if err := st.MarkReplicaFailed(ReplicaKindRun, "run-1", t0, "boom"); err != nil {
		t.Fatalf("MarkReplicaFailed: %v", err)
	}
	n, err := st.RetryReplicaFailed()
	if err != nil || n != 1 {
		t.Fatalf("RetryReplicaFailed = %d, %v; want 1, nil", n, err)
	}
	if due, err := st.DueReplicaItems(t0, 0); err != nil || len(due) != 1 {
		t.Fatalf("retried item should be due immediately: %v, %v", due, err)
	}
}

func TestResetReplicaSyncingReclaimsInterruptedTransfers(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindTree, "runs", t0)
	if err := st.MarkReplicaSyncing(ReplicaKindTree, "runs", t0); err != nil {
		t.Fatalf("MarkReplicaSyncing: %v", err)
	}
	// Nothing is transferring it any more — the process that was doing so is gone.
	if err := st.ResetReplicaSyncing(); err != nil {
		t.Fatalf("ResetReplicaSyncing: %v", err)
	}
	if it := mustItem(t, st, ReplicaKindTree, "runs"); it.Status != ReplicaPending {
		t.Fatalf("status = %q, want pending", it.Status)
	}
}

func TestReplicaLinkRemembersWhenTheOutageStarted(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()

	if l, err := st.ReplicaLinkState(); err != nil || !l.Healthy() {
		t.Fatalf("a fresh database must not claim an outage: %+v, %v", l, err)
	}
	if err := st.RecordReplicaError(t0, "no route to host"); err != nil {
		t.Fatalf("RecordReplicaError: %v", err)
	}
	if err := st.RecordReplicaError(t0.Add(10*time.Minute), "no route to host"); err != nil {
		t.Fatalf("RecordReplicaError: %v", err)
	}
	l, err := st.ReplicaLinkState()
	if err != nil {
		t.Fatalf("ReplicaLinkState: %v", err)
	}
	if l.Healthy() {
		t.Fatal("link should be reported as down")
	}
	// Restarting the clock on every retry would turn a six-hour outage into a two-minute
	// one, which is exactly the number an operator would act on.
	if !l.DownSince.Equal(t0.Truncate(time.Millisecond)) {
		t.Fatalf("down_since = %v, want the first failure at %v", l.DownSince, t0)
	}
	if !l.LastErrAt.Equal(t0.Add(10 * time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("last_err_at = %v, want the latest failure", l.LastErrAt)
	}

	if err := st.RecordReplicaOK(t0.Add(time.Hour)); err != nil {
		t.Fatalf("RecordReplicaOK: %v", err)
	}
	if l, err := st.ReplicaLinkState(); err != nil || !l.Healthy() {
		t.Fatalf("link should be healthy again: %+v, %v", l, err)
	}
}

func TestReplicaQueueCounts(t *testing.T) {
	st, _ := openTestStore(t)
	t0 := time.Now()
	mustEnqueue(t, st, ReplicaKindRun, "run-1", t0)
	mustEnqueue(t, st, ReplicaKindRun, "run-2", t0.Add(time.Minute))
	mustEnqueue(t, st, ReplicaKindRun, "run-3", t0.Add(2*time.Minute))
	if err := st.MarkReplicaDone(ReplicaKindRun, "run-1", t0, 10); err != nil {
		t.Fatalf("MarkReplicaDone: %v", err)
	}
	if err := st.MarkReplicaFailed(ReplicaKindRun, "run-3", t0, "boom"); err != nil {
		t.Fatalf("MarkReplicaFailed: %v", err)
	}
	c, err := st.ReplicaQueueCounts()
	if err != nil {
		t.Fatalf("ReplicaQueueCounts: %v", err)
	}
	if c.Done != 1 || c.Pending != 1 || c.Failed != 1 {
		t.Fatalf("counts = %+v, want 1 done / 1 pending / 1 failed", c)
	}
	// The oldest *unfinished* item, not the oldest row: run-1 is already off-site.
	if !c.OldestPendingAt.Equal(t0.Add(time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("oldest_pending_at = %v, want run-2's queue time", c.OldestPendingAt)
	}
}

func TestHasActiveRun(t *testing.T) {
	st, _ := openTestStore(t)
	run, err := st.CreateRun(&Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if busy, err := st.HasActiveRun("files-web01"); err != nil || !busy {
		t.Fatalf("HasActiveRun during a run = %v, %v; want true", busy, err)
	}
	if busy, err := st.HasActiveRun("other"); err != nil || busy {
		t.Fatalf("HasActiveRun for an idle instance = %v, %v; want false", busy, err)
	}
	if err := st.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if busy, err := st.HasActiveRun("files-web01"); err != nil || busy {
		t.Fatalf("HasActiveRun after the run finished = %v, %v; want false", busy, err)
	}
}

// A run carries the state of whatever carries its data: its own directory for a dump,
// the repository for a restic backup. Both have to reach the run list, since that is
// where an operator looks to see whether a backup is off-site.
func TestRunCarriesItsReplicaStatus(t *testing.T) {
	st, _ := openTestStore(t)
	now := time.Now()
	dump := storeDump(t, st, "mysql-web01", now, 16)
	repoRun, err := st.CreateRun(&Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.FinishRun(repoRun.ID, now, 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// Never queued: unknown, not failed.
	if r, err := st.Run(dump.ID); err != nil || r.ReplicaStatus != "" {
		t.Fatalf("un-queued run reports %q, want empty", r.ReplicaStatus)
	}

	mustEnqueue(t, st, ReplicaKindRun, dump.ID, now)
	if err := st.MarkReplicaDone(ReplicaKindRun, dump.ID, now, 16); err != nil {
		t.Fatalf("MarkReplicaDone: %v", err)
	}
	mustEnqueue(t, st, ReplicaKindRepo, "files-web01", now)

	got := map[string]ReplicaStatus{}
	runs, err := st.ListRuns(0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, r := range runs {
		got[r.ID] = r.ReplicaStatus
	}
	if got[dump.ID] != ReplicaDone {
		t.Fatalf("dump run reports %q, want done", got[dump.ID])
	}
	if got[repoRun.ID] != ReplicaPending {
		t.Fatalf("restic run reports %q, want its repository's pending", got[repoRun.ID])
	}
}

// Replication must not be able to change what a backup's history says. A link that is
// down turns into a queue row, never into a run that reads as failed.
func TestReplicaFailureLeavesRunStatusAlone(t *testing.T) {
	st, _ := openTestStore(t)
	now := time.Now()
	run := storeDump(t, st, "mysql-web01", now, 32)
	mustEnqueue(t, st, ReplicaKindRun, run.ID, now)
	if err := st.MarkReplicaFailed(ReplicaKindRun, run.ID, now, "connection to the replica failed"); err != nil {
		t.Fatalf("MarkReplicaFailed: %v", err)
	}
	if err := st.RecordReplicaError(now, "connection to the replica failed"); err != nil {
		t.Fatalf("RecordReplicaError: %v", err)
	}
	after, err := st.Run(run.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if after.Status != StatusSuccess {
		t.Fatalf("run status = %q after a replication failure, want success", after.Status)
	}
	if after.Err != "" {
		t.Fatalf("run err = %q after a replication failure, want empty", after.Err)
	}
	if after.ReplicaStatus != ReplicaFailed {
		t.Fatalf("replica status = %q, want failed", after.ReplicaStatus)
	}
}

func TestReplicableRunsSkipsRotatedAndUnsuccessful(t *testing.T) {
	st, _ := openTestStore(t)
	now := time.Now()
	good := storeDump(t, st, "mysql-web01", now, 8)
	rotated := storeDump(t, st, "mysql-web01", now.Add(-time.Hour), 8)
	if _, err := st.DeleteRunPayload(rotated.ID); err != nil {
		t.Fatalf("DeleteRunPayload: %v", err)
	}
	failed, err := st.CreateRun(&Instance{ID: "mysql-web01", Script: "mysql-backup", RunnerID: "db-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.FinishRun(failed.ID, now, 1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	ids, err := st.ReplicableRuns(0)
	if err != nil {
		t.Fatalf("ReplicableRuns: %v", err)
	}
	if len(ids) != 1 || ids[0] != good.ID {
		t.Fatalf("ReplicableRuns = %v, want only %s", ids, good.ID)
	}
}

func TestSnapshotDatabaseIsReadable(t *testing.T) {
	st, dir := openTestStore(t)
	if _, err := st.CreateRun(&Instance{ID: "mysql-web01", Script: "mysql-backup", RunnerID: "db-01"}, "", 60); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	path := dir + "/snapshot.db"
	if err := st.SnapshotDatabase(path); err != nil {
		t.Fatalf("SnapshotDatabase: %v", err)
	}
	// Taking it twice must work: the replica gets a fresh snapshot on every meta pass,
	// and VACUUM INTO refuses to overwrite.
	if err := st.SnapshotDatabase(path); err != nil {
		t.Fatalf("second SnapshotDatabase: %v", err)
	}
	snap, err := Open(path, dir, nil)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	runs, err := snap.ListRuns(0)
	if err != nil {
		t.Fatalf("ListRuns on the snapshot: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("snapshot holds %d runs, want 1", len(runs))
	}
}

func mustEnqueue(t *testing.T, st *Store, kind ReplicaKind, key string, now time.Time) {
	t.Helper()
	if err := st.EnqueueReplica(kind, key, now); err != nil {
		t.Fatalf("EnqueueReplica(%s, %s): %v", kind, key, err)
	}
}

func mustItem(t *testing.T, st *Store, kind ReplicaKind, key string) *ReplicaItem {
	t.Helper()
	it, err := st.ReplicaItemFor(kind, key)
	if err != nil {
		t.Fatalf("ReplicaItemFor(%s, %s): %v", kind, key, err)
	}
	if it == nil {
		t.Fatalf("no queue row for %s:%s", kind, key)
	}
	return it
}
