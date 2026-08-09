package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"arcatum/pkg/config"
)

// End-to-end over the real rsync binary, with the "remote" reached through a local shell
// instead of ssh. Nothing here is a stand-in for rsync itself: the flags, the resume
// behaviour and the delete ceiling are the parts that decide whether an off-site copy is
// worth having, and a fake would assert only that the code calls what it calls.

// replicaTestServer builds a server whose replica points at a directory on this machine.
func replicaTestServer(t *testing.T, mirror bool) (*Server, string, string) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
	st, dir := openTestStore(t)
	dest := filepath.Join(dir, "replica")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	// rsync runs "$RSYNC_RSH <host> rsync --server …"; dropping the host argument and
	// executing the rest performs the same protocol over a pipe.
	rsh := filepath.Join(dir, "local-rsh.sh")
	if err := os.WriteFile(rsh, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatalf("write rsh: %v", err)
	}
	old := rshCommand
	rshCommand = func(config.Replica) string { return rsh }
	t.Cleanup(func() { rshCommand = old })

	cfg := config.Replica{
		Enabled: true, Host: "localhost", Path: dest,
		Mirror: mirror, MaxDelete: 100,
	}
	srv := &Server{
		store:   st,
		log:     log.New(io.Discard, "", 0),
		sched:   NewScheduler(time.UTC),
		catalog: &Catalog{byName: map[string]*ScriptEntry{}},
		logins:  newLoginLimiter(),
	}
	bin, err := exec.LookPath("rsync")
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	srv.replica = &replicator{
		srv: srv, cfg: cfg, bin: bin,
		timeout: 30 * time.Second, sweepEvery: time.Hour, probeEvery: time.Hour,
		wake: make(chan struct{}, 1),
	}
	return srv, dir, dest
}

func TestReplicaTransfersAFinishedDump(t *testing.T) {
	srv, _, dest := replicaTestServer(t, true)
	run := storeDump(t, srv.store, "mysql-web01", time.Now(), 4096)

	srv.enqueueRunForReplica(run.ID)
	srv.replica.drain(context.Background())

	copied := filepath.Join(dest, "runs", run.ID, "data.bin")
	fi, err := os.Stat(copied)
	if err != nil {
		t.Fatalf("dump did not reach the replica: %v", err)
	}
	if fi.Size() != 4096 {
		t.Fatalf("copied %d bytes, want 4096", fi.Size())
	}
	if it := mustItem(t, srv.store, ReplicaKindRun, run.ID); it.Status != ReplicaDone {
		t.Fatalf("queue row = %q, want done", it.Status)
	}
	if link, err := srv.store.ReplicaLinkState(); err != nil || !link.Healthy() {
		t.Fatalf("a successful transfer must clear the link state: %+v, %v", link, err)
	}
}

func TestReplicaNeverCopiesAnUploadInProgress(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, true)
	run, err := srv.store.CreateRun(&Instance{ID: "mysql-web01", Script: "mysql-backup", RunnerID: "db-01"}, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f, err := srv.store.CreateData(run.ID)
	if err != nil {
		t.Fatalf("CreateData: %v", err)
	}
	if _, err := f.Write(make([]byte, 512)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	// The upload has not been committed: this is data.part, not a backup.
	if _, err := os.Stat(filepath.Join(dir, "backup", "runs", run.ID, "data.part")); err != nil {
		t.Fatalf("expected an in-flight upload on disk: %v", err)
	}

	mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
	srv.replica.drain(context.Background())

	if _, err := os.Stat(filepath.Join(dest, "runs", run.ID, "data.part")); !os.IsNotExist(err) {
		t.Fatalf("a half-uploaded dump must not reach the replica (err=%v)", err)
	}
}

func TestReplicaMirrorsDeletionsAndDropsTheQueueRow(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, true)
	run := storeDump(t, srv.store, "mysql-web01", time.Now(), 128)
	mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
	srv.replica.drain(context.Background())
	if _, err := os.Stat(filepath.Join(dest, "runs", run.ID, "data.bin")); err != nil {
		t.Fatalf("setup: dump did not reach the replica: %v", err)
	}

	// Retention removes the run directory here. No event is raised — reconciling the
	// whole tree is what carries that through to the replica.
	if err := os.RemoveAll(filepath.Join(dir, "backup", "runs", run.ID)); err != nil {
		t.Fatalf("remove run dir: %v", err)
	}
	mustEnqueue(t, srv.store, ReplicaKindTree, "runs", time.Now())
	mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
	srv.replica.drain(context.Background())

	if _, err := os.Stat(filepath.Join(dest, "runs", run.ID)); !os.IsNotExist(err) {
		t.Fatalf("mirroring should have removed the run from the replica (err=%v)", err)
	}
	// The row described something that no longer exists here, so it goes too.
	it, err := srv.store.ReplicaItemFor(ReplicaKindRun, run.ID)
	if err != nil {
		t.Fatalf("ReplicaItemFor: %v", err)
	}
	if it != nil {
		t.Fatalf("queue row for a removed run should be gone, got %+v", it)
	}
}

func TestReplicaWithoutMirrorKeepsWhatWasDeletedHere(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, false)
	run := storeDump(t, srv.store, "mysql-web01", time.Now(), 128)
	mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
	srv.replica.drain(context.Background())

	if err := os.RemoveAll(filepath.Join(dir, "backup", "runs", run.ID)); err != nil {
		t.Fatalf("remove run dir: %v", err)
	}
	mustEnqueue(t, srv.store, ReplicaKindTree, "runs", time.Now())
	srv.replica.drain(context.Background())

	if _, err := os.Stat(filepath.Join(dest, "runs", run.ID, "data.bin")); err != nil {
		t.Fatalf("without mirroring the off-site copy must survive a deletion here: %v", err)
	}
}

// The guard between an unmounted backup_dir and an emptied replica. It is the one thing
// that makes mirroring safe to switch on, so it is asserted against the real rsync.
func TestReplicaRefusesToDeleteMoreThanTheCeiling(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, true)
	srv.replica.cfg.MaxDelete = 2

	backupRuns := filepath.Join(dir, "backup", "runs")
	for i := 0; i < 6; i++ {
		run := storeDump(t, srv.store, "mysql-web01", time.Now(), 64)
		mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
		_ = run
	}
	mustEnqueue(t, srv.store, ReplicaKindTree, "runs", time.Now())
	srv.replica.drain(context.Background())
	before, err := os.ReadDir(filepath.Join(dest, "runs"))
	if err != nil || len(before) != 6 {
		t.Fatalf("setup: replica holds %d runs, want 6 (%v)", len(before), err)
	}

	// backup_dir has gone missing — an unmounted volume looks exactly like this.
	if err := os.RemoveAll(backupRuns); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(backupRuns, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustEnqueue(t, srv.store, ReplicaKindTree, "runs", time.Now())
	srv.replica.drain(context.Background())

	after, err := os.ReadDir(filepath.Join(dest, "runs"))
	if err != nil {
		t.Fatalf("read replica: %v", err)
	}
	if len(after) != 6 {
		t.Fatalf("the ceiling did not hold: replica now has %d runs, want all 6 left alone", len(after))
	}
	it := mustItem(t, srv.store, ReplicaKindTree, "runs")
	if it.Status != ReplicaFailed {
		t.Fatalf("a refused pass must be recorded as failed, got %q", it.Status)
	}
}

func TestReplicaDefersARepositoryWhileItsInstanceIsBackingUp(t *testing.T) {
	srv, dir, _ := replicaTestServer(t, true)
	repo := filepath.Join(dir, "backup", "restic", "files-web01")
	if err := os.MkdirAll(filepath.Join(repo, "data"), 0o750); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if _, err := srv.store.CreateRun(&Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}, "", 3600); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	mustEnqueue(t, srv.store, ReplicaKindRepo, "files-web01", time.Now())
	srv.replica.drain(context.Background())

	it := mustItem(t, srv.store, ReplicaKindRepo, "files-web01")
	if it.Status != ReplicaPending {
		t.Fatalf("status = %q, want pending: a repository mid-backup is deferred, not failed", it.Status)
	}
	if it.Attempts != 0 {
		t.Fatalf("deferring must not count as a failed attempt, got %d", it.Attempts)
	}
	if !it.NextTryAt.After(time.Now()) {
		t.Fatalf("deferred item should be scheduled for later, got %v", it.NextTryAt)
	}
}

// A broken link must not be able to touch the backups here. This is the invariant the
// whole subsystem is built around, so it is asserted end to end rather than by reading
// the code.
func TestBrokenLinkLeavesTheBackupIntact(t *testing.T) {
	srv, dir, _ := replicaTestServer(t, true)
	// Point the replica at a path under a file, so rsync cannot possibly write there.
	srv.replica.cfg.Path = filepath.Join(dir, "backup", "runs", "nope", "deeper")
	run := storeDump(t, srv.store, "mysql-web01", time.Now(), 256)
	local := filepath.Join(dir, "backup", "runs", run.ID, "data.bin")
	rsh := filepath.Join(dir, "local-rsh.sh")
	rshCommand = func(config.Replica) string { return rsh + " /nonexistent-rsync" }

	mustEnqueue(t, srv.store, ReplicaKindRun, run.ID, time.Now())
	srv.replica.drain(context.Background())

	if _, err := os.Stat(local); err != nil {
		t.Fatalf("the local backup must be untouched by a failed transfer: %v", err)
	}
	after, err := srv.store.Run(run.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if after.Status != StatusSuccess || after.Err != "" {
		t.Fatalf("a replication failure changed the run: status=%q err=%q", after.Status, after.Err)
	}
	it := mustItem(t, srv.store, ReplicaKindRun, run.ID)
	if it.Status != ReplicaFailed || it.Err == "" {
		t.Fatalf("the failure belongs in the queue with a reason, got %+v", it)
	}
}

// Without the database and the keys, a replica full of restic repositories is a pile of
// files nobody can open. This is what makes it a restore point instead.
func TestReplicaCarriesTheDatabaseAndTheKeys(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, true)
	master := filepath.Join(dir, "secrets-master.key")
	if err := os.WriteFile(master, []byte("not really a key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	cfgPath := filepath.Join(dir, "server.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nlisten = \":8443\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.configPath = cfgPath
	srv.replica.cfg.IncludeKeys = true
	srv.replica.keyFiles = []string{master, filepath.Join(dir, "missing.key")}
	storeDump(t, srv.store, "mysql-web01", time.Now(), 32)

	mustEnqueue(t, srv.store, ReplicaKindMeta, metaKey, time.Now())
	srv.replica.drain(context.Background())

	if it := mustItem(t, srv.store, ReplicaKindMeta, metaKey); it.Status != ReplicaDone {
		t.Fatalf("meta item = %q (%s), want done", it.Status, it.Err)
	}
	// The snapshot has to be a database, not a copy of a file being written to.
	snapPath := filepath.Join(dest, "meta", "arcatum.db")
	snap, err := Open(snapPath, dir, nil)
	if err != nil {
		t.Fatalf("replicated database is not readable: %v", err)
	}
	defer snap.Close()
	if runs, err := snap.ListRuns(0); err != nil || len(runs) != 1 {
		t.Fatalf("replicated database holds %d runs (%v), want 1", len(runs), err)
	}
	if _, err := os.Stat(filepath.Join(dest, "meta", "server.toml")); err != nil {
		t.Fatalf("server.toml did not reach the replica: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dest, "keys", "secrets-master.key"))
	if err != nil {
		t.Fatalf("master key did not reach the replica: %v", err)
	}
	// A key that lands world-readable on the replica is a key that has leaked.
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("replicated key mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestReplicaSkipsKeysWhenNotConfigured(t *testing.T) {
	srv, dir, dest := replicaTestServer(t, true)
	master := filepath.Join(dir, "secrets-master.key")
	if err := os.WriteFile(master, []byte("not really a key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	srv.replica.cfg.IncludeKeys = false
	srv.replica.keyFiles = []string{master}

	mustEnqueue(t, srv.store, ReplicaKindMeta, metaKey, time.Now())
	srv.replica.drain(context.Background())

	if _, err := os.Stat(filepath.Join(dest, "keys")); !os.IsNotExist(err) {
		t.Fatalf("keys must not travel unless asked for (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "meta", "arcatum.db")); err != nil {
		t.Fatalf("the database should still be replicated: %v", err)
	}
}

func TestReplicaStatusEndpointAndRoles(t *testing.T) {
	srv, _, _ := replicaTestServer(t, true)
	mustUser(t, srv, "boss", "correct-horse-battery", "admin")
	mustUser(t, srv, "looker", "correct-horse-battery", "viewer")
	adminCookie := login(t, srv, "boss", "correct-horse-battery")
	viewerCookie := login(t, srv, "looker", "correct-horse-battery")

	// Seeing that the off-site copy has stopped working is not an administrative act.
	res := webCall(t, srv, "GET", "/api/v1/replica", nil, viewerCookie)
	if res.Code != 200 {
		t.Fatalf("viewer GET /replica = %d, want 200", res.Code)
	}
	var rep ReplicaReport
	if err := json.Unmarshal(res.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rep.Enabled || rep.Target == "" {
		t.Fatalf("report = %+v, want an enabled replica with a target", rep)
	}

	// Making the server do something is.
	if res := webCall(t, srv, "POST", "/api/v1/replica/sync", nil, viewerCookie); res.Code != 403 {
		t.Fatalf("viewer POST /replica/sync = %d, want 403", res.Code)
	}
	if res := webCall(t, srv, "POST", "/api/v1/replica/retry", nil, viewerCookie); res.Code != 403 {
		t.Fatalf("viewer POST /replica/retry = %d, want 403", res.Code)
	}
	if res := webCall(t, srv, "POST", "/api/v1/replica/retry", nil, adminCookie); res.Code != 200 {
		t.Fatalf("admin POST /replica/retry = %d, want 200", res.Code)
	}
}

func TestReplicaStatusReportsDisabled(t *testing.T) {
	srv := webServer(t)
	rec := httptest.NewRecorder()
	srv.handleReplicaStatus(rec, httptest.NewRequest("GET", "/api/v1/replica", nil))
	var rep ReplicaReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Enabled {
		t.Fatal("a server without [replica] must report replication as off")
	}
	// Nothing configured is not the same as something broken.
	if !rep.Healthy {
		t.Fatal("an unconfigured replica must not read as an outage")
	}
}

func TestStartReplicationIsInertWhenSwitchedOff(t *testing.T) {
	srv := webServer(t)
	// The whole point: a server with no replica configured starts and behaves normally.
	srv.StartReplication(context.Background())
	if srv.replica != nil {
		t.Fatal("replica should stay nil")
	}
}
