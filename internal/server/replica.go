package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arcatum/pkg/config"
	"arcatum/pkg/proto"
)

// Off-site replication.
//
// Everything the server stores is pushed to a second machine, so that this one stops
// being the only place the backups exist. Three properties shape the design, and all
// three come straight from what an off-site copy is for:
//
//   - The link will break. So the unit of work is a queue row, not an event: an item
//     that cannot be sent keeps its place and is retried, and a link that comes back
//     finds the backlog waiting. Nothing is ever dropped because a transfer failed.
//   - The break has to be visible. So the queue is in the database rather than in
//     memory, and every run carries the state of whatever carries its data. An outage
//     that only shows up as backups quietly not being off-site is worse than no replica
//     at all, because it is believed.
//   - It must not endanger the backups here. So replication only ever reads backup_dir,
//     runs one transfer at a time at a lowered priority, and records its failures in its
//     own tables. A broken replica cannot fail a backup, slow one down, or delete one:
//     the only deletions it performs are on the far side, under a --max-delete ceiling.
//
// The subsystem is inert when [replica] is not configured, and a missing rsync binary
// disables it with one log line rather than stopping the server. Replication is the last
// thing that should keep a backup server from starting.

// replicaWorkerEvery is how often the queue is checked when nothing has woken it. New
// work nudges the worker directly, so this is only the floor for retries coming out of
// backoff.
const replicaWorkerEvery = 30 * time.Second

// replicaBusyRetry is how long a repository waits when its instance is mid-backup.
const replicaBusyRetry = 5 * time.Minute

// replicaStagingDir is where the database snapshot and server.toml are assembled before
// being sent. It lives under backup_dir so it shares the volume it is a copy of.
const replicaStagingDir = "replica-staging"

// ReplicaOptions wires the configured replica into the server.
type ReplicaOptions struct {
	config.Replica
	// KeyFiles are the key material replicated when IncludeKeys is set: the CA, the
	// server certificate, the dispatch signing key and the secrets master key, including
	// the predecessors kept during a rotation. The list is built from the loaded
	// configuration rather than from a fixed directory, so a rotation that introduces a
	// new file cannot silently stop being replicated.
	KeyFiles []string
}

// replicator owns the queue worker. It is nil on a server with no [replica] section.
type replicator struct {
	srv      *Server
	cfg      config.Replica
	keyFiles []string
	bin      string // resolved rsync path

	timeout    time.Duration
	sweepEvery time.Duration
	probeEvery time.Duration

	// wake nudges the worker when something has been queued, so a backup is off-site in
	// seconds rather than at the next tick. Buffered and non-blocking: a nudge that finds
	// the worker already awake is redundant, not something to wait for.
	wake chan struct{}
}

// StartReplication brings up the off-site copy until ctx is cancelled. It is safe to
// call when replication is switched off, in which case it does nothing.
func (s *Server) StartReplication(ctx context.Context) {
	if s.replica == nil {
		return
	}
	r := s.replica
	bin, err := exec.LookPath("rsync")
	if err != nil {
		// Deliberately not fatal. A backup server that refuses to start because its
		// second copy is unavailable has traded a degraded system for no system.
		s.log.Printf("replica: rsync is not installed — off-site replication is disabled " +
			"(install rsync and restart)")
		_ = s.store.RecordReplicaError(time.Now(), "rsync is not installed on the server")
		return
	}
	r.bin = bin
	if err := s.store.ResetReplicaSyncing(); err != nil {
		s.log.Printf("replica: %v", err)
	}
	s.log.Printf("replica: off-site copy to %s every %s (mirror=%v, keys=%v)",
		r.cfg.Addr(), r.sweepEvery, r.cfg.Mirror, r.cfg.IncludeKeys)
	if r.cfg.IncludeKeys {
		s.log.Printf("  WARNING: the replica receives the PKI and the secrets master key. " +
			"Whoever reaches its directory can open every repository and issue certificates — " +
			"keep it mode 0700 under a dedicated account.")
	}
	if r.cfg.KnownHosts == "" {
		s.log.Printf("  WARNING: [replica] known_hosts is not set — the replica's host key is not verified.")
	}

	go r.workerLoop(ctx)
	go r.sweepLoop(ctx)
	go r.probeLoop(ctx)
}

// workerLoop transfers due items, one at a time.
func (r *replicator) workerLoop(ctx context.Context) {
	t := time.NewTicker(replicaWorkerEvery)
	defer t.Stop()
	for {
		r.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
	}
}

// drain transfers everything currently due. One item at a time on purpose: several
// parallel transfers over one tunnel finish no sooner and take I/O away from the backups
// still arriving.
func (r *replicator) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := r.srv.store.DueReplicaItems(time.Now(), 1)
		if err != nil {
			r.srv.log.Printf("replica: read queue: %v", err)
			return
		}
		if len(items) == 0 {
			return
		}
		r.transfer(ctx, items[0])
	}
}

// transfer performs one queue item.
func (r *replicator) transfer(ctx context.Context, it *ReplicaItem) {
	st := r.srv.store
	now := time.Now()

	jobs, err := r.jobsFor(it)
	switch {
	case errors.Is(err, errReplicaItemGone):
		// What this row pointed at is no longer here. The mirroring pass is what removes
		// it from the replica; the row itself has nothing left to describe.
		if err := st.DeleteReplicaItem(it.Kind, it.Key); err != nil {
			r.srv.log.Printf("replica: %s=%s: %v", it.Kind, it.Key, err)
		}
		return
	case errors.Is(err, errReplicaItemBusy):
		if err := st.DeferReplicaItem(it.Kind, it.Key, now.Add(replicaBusyRetry)); err != nil {
			r.srv.log.Printf("replica: %s=%s: %v", it.Kind, it.Key, err)
		}
		return
	case err != nil:
		r.fail(it, now, err)
		return
	}

	if err := st.MarkReplicaSyncing(it.Kind, it.Key, now); err != nil {
		r.srv.log.Printf("replica: %s=%s: %v", it.Kind, it.Key, err)
		return
	}
	var total int64
	for _, job := range jobs {
		n, err := runRsync(ctx, r.bin, r.cfg, job, r.timeout)
		total += n
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down. Leave the row for the next start rather than recording
				// a failure nobody caused.
				_ = st.DeferReplicaItem(it.Kind, it.Key, time.Now())
				return
			}
			r.fail(it, time.Now(), err)
			return
		}
	}
	if err := st.MarkReplicaDone(it.Kind, it.Key, time.Now(), total); err != nil {
		r.srv.log.Printf("replica: %s=%s: %v", it.Kind, it.Key, err)
		return
	}
	if err := st.RecordReplicaOK(time.Now()); err != nil {
		r.srv.log.Printf("replica: %v", err)
	}
	r.srv.log.Printf("replica: %s=%s copied (%d bytes)", it.Kind, it.Key, total)
}

// fail records a failed transfer. A failure of the link is also recorded against the
// connection, so the UI can distinguish "the replica is down" from "one item is stuck".
func (r *replicator) fail(it *ReplicaItem, now time.Time, cause error) {
	msg := cause.Error()
	if err := r.srv.store.MarkReplicaFailed(it.Kind, it.Key, now, msg); err != nil {
		r.srv.log.Printf("replica: %s=%s: %v", it.Kind, it.Key, err)
	}
	if errors.Is(cause, errReplicaUnreachable) {
		if err := r.srv.store.RecordReplicaError(now, msg); err != nil {
			r.srv.log.Printf("replica: %v", err)
		}
	}
	r.srv.log.Printf("replica: %s=%s failed: %v", it.Kind, it.Key, cause)
}

// errReplicaItemGone says the source no longer exists here.
var errReplicaItemGone = errors.New("source no longer present")

// errReplicaItemBusy says the source is being written to right now.
var errReplicaItemBusy = errors.New("source is in use")

// jobsFor turns a queue item into the transfers that carry it.
func (r *replicator) jobsFor(it *ReplicaItem) ([]rsyncJob, error) {
	backupDir := r.srv.store.backupDir
	switch it.Kind {
	case ReplicaKindRun:
		if !dirExists(filepath.Join(backupDir, "runs", it.Key)) {
			return nil, errReplicaItemGone
		}
		return []rsyncJob{{
			Src: filepath.Join(backupDir, "runs"), Dst: "runs",
			Include: []string{"/" + it.Key + "/***"}, Delete: true,
		}}, nil

	case ReplicaKindRepo:
		if !dirExists(filepath.Join(backupDir, "restic", it.Key)) {
			return nil, errReplicaItemGone
		}
		busy, err := r.srv.store.HasActiveRun(it.Key)
		if err != nil {
			return nil, err
		}
		if busy {
			return nil, errReplicaItemBusy
		}
		return resticRepoJobs(filepath.Join(backupDir, "restic"), "restic", it.Key), nil

	case ReplicaKindTree:
		return r.treeJobs(it.Key)

	case ReplicaKindMeta:
		return r.metaJobs()
	}
	return nil, fmt.Errorf("unknown replica item kind %q", it.Kind)
}

// resticRepoJobs copies repositories in two passes: the packs and keys first, the index
// and snapshots after. instance narrows it to one repository; empty covers them all,
// which is what the hourly reconciliation does.
//
// The order is what makes an interrupted copy still usable. A snapshot names an index,
// an index names packs; arriving in the other order leaves the replica holding a
// snapshot that points at data which is not there yet — a repository restic will open
// and then fail to restore from, which is the worst of the possible states because it
// looks like a backup.
func resticRepoJobs(src, dst, instance string) []rsyncJob {
	scope, include := "*", []string(nil)
	if instance != "" {
		scope = "/" + instance
		include = []string{"/" + instance + "/***"}
	}
	return []rsyncJob{
		{Src: src, Dst: dst, Include: include,
			Excludes: []string{scope + "/index/", scope + "/snapshots/"}},
		{Src: src, Dst: dst, Include: include, Delete: true},
	}
}

// treeJobs reconciles a whole subtree. This is what carries retention through to the
// replica: a dump deleted here raises no event, and the queue would never hear about it,
// so once an hour the trees are compared wholesale. It is also the safety net for
// anything the per-item path missed.
func (r *replicator) treeJobs(name string) ([]rsyncJob, error) {
	src := filepath.Join(r.srv.store.backupDir, name)
	if !dirExists(src) {
		// Nothing to reconcile yet — a server that has not produced any restic backup has
		// no restic/ directory. Not an error, and emphatically not a reason to mirror an
		// empty directory onto a replica that may have one full of backups.
		return nil, errReplicaItemGone
	}
	if name == "restic" {
		return resticRepoJobs(src, name, ""), nil
	}
	return []rsyncJob{{Src: src, Dst: name, Delete: true}}, nil
}

// metaJobs assembles what makes the replica a restore point rather than a pile of
// undecryptable files: a consistent snapshot of the database, the server configuration
// for reference, and — when configured — the keys.
func (r *replicator) metaJobs() ([]rsyncJob, error) {
	staging := filepath.Join(r.srv.store.backupDir, replicaStagingDir)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	if err := r.srv.store.SnapshotDatabase(filepath.Join(staging, "arcatum.db")); err != nil {
		return nil, err
	}
	if r.srv.configPath != "" {
		if err := copyFileMode(r.srv.configPath, filepath.Join(staging, "server.toml"), 0o600); err != nil {
			return nil, fmt.Errorf("stage server.toml: %w", err)
		}
	}
	jobs := []rsyncJob{{Src: staging, Dst: "meta", Delete: true, Chmod: "D700,F600"}}

	if r.cfg.IncludeKeys && len(r.keyFiles) > 0 {
		// The key files are sent from where they live, not staged: a private key copied
		// into backup_dir would end up on the same volume as the repositories it
		// unlocks, which is precisely what the production layout keeps apart.
		//
		// No --delete here. Removing a key from the replica is not something to automate:
		// during a rotation the predecessor is exactly what a restore may still need.
		files := make([]string, 0, len(r.keyFiles))
		for _, f := range r.keyFiles {
			if fileExists(f) {
				files = append(files, f)
			}
		}
		if len(files) > 0 {
			jobs = append(jobs, rsyncJob{Files: files, Dst: "keys", Chmod: "D700,F600"})
		}
	}
	return jobs, nil
}

// sweepLoop re-queues anything that is not off-site and reconciles the trees. It runs
// once at start, because the most likely reason for a backlog is that this server was
// down, or its link was, while backups kept being produced.
func (r *replicator) sweepLoop(ctx context.Context) {
	t := time.NewTicker(r.sweepEvery)
	defer t.Stop()
	r.sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep()
		}
	}
}

// sweep queues everything that should be off-site. Enqueueing is idempotent, so an item
// already waiting stays as it is and one already sent is queued again only to be
// re-checked by rsync, which for unchanged content costs a directory listing.
func (r *replicator) sweep() {
	st, now := r.srv.store, time.Now()
	enqueue := func(kind ReplicaKind, key string) {
		if err := st.EnqueueReplica(kind, key, now); err != nil {
			r.srv.log.Printf("replica: queue %s=%s: %v", kind, key, err)
		}
	}
	runs, err := st.ReplicableRuns(0)
	if err != nil {
		r.srv.log.Printf("replica: %v", err)
	}
	for _, id := range runs {
		enqueue(ReplicaKindRun, id)
	}
	instances, err := st.Instances()
	if err != nil {
		r.srv.log.Printf("replica: %v", err)
	}
	for _, in := range instances {
		if dirExists(filepath.Join(st.backupDir, "restic", in.ID)) {
			enqueue(ReplicaKindRepo, in.ID)
		}
	}
	for _, tree := range []string{"runs", "restic", "config-backups"} {
		// A tree that does not exist here yet is not queued at all — a server that has
		// never produced a restic backup has no restic/, and queueing it would only mean
		// a row created and dropped again on every sweep.
		if dirExists(filepath.Join(st.backupDir, tree)) {
			enqueue(ReplicaKindTree, tree)
		}
	}
	enqueue(ReplicaKindMeta, metaKey)
	r.nudge()
}

// nudge wakes the worker without blocking if it is already awake.
func (r *replicator) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// probeLoop checks the replica answers even when there is nothing to send, so an outage
// is visible from the moment it starts rather than from the next backup.
func (r *replicator) probeLoop(ctx context.Context) {
	t := time.NewTicker(r.probeEvery)
	defer t.Stop()
	r.probe(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.probe(ctx)
		}
	}
}

func (r *replicator) probe(ctx context.Context) {
	now := time.Now()
	if err := probeReplica(ctx, r.cfg); err != nil {
		if ctx.Err() != nil {
			return
		}
		if err := r.srv.store.RecordReplicaError(now, err.Error()); err != nil {
			r.srv.log.Printf("replica: %v", err)
		}
		return
	}
	if err := r.srv.store.RecordReplicaOK(now); err != nil {
		r.srv.log.Printf("replica: %v", err)
	}
}

// enqueueRunForReplica queues a finished run's output for the off-site copy. Called off
// the update stream, like dump retention, because the runner is still holding that
// request open and this may touch the disk.
//
// Errors are logged and dropped: a run that could not be queued is picked up by the next
// sweep, and there is nothing about replication that should reach back into a backup
// that has already succeeded.
func (s *Server) enqueueRunForReplica(runID string) {
	if s.replica == nil {
		return
	}
	run, err := s.store.Run(runID)
	if err != nil || run == nil || run.Status != StatusSuccess {
		return
	}
	now := time.Now()
	if err := s.store.EnqueueReplica(ReplicaKindRun, runID, now); err != nil {
		s.log.Printf("replica: queue run=%s: %v", runID, err)
	}
	// A restic backup leaves nothing in the run directory but a log — what it produced
	// went into the repository, so that is what has to travel.
	if entry, ok := s.catalog.Get(run.Script); ok && entry.Manifest.Type == proto.TypeRestic {
		if err := s.store.EnqueueReplica(ReplicaKindRepo, run.InstanceID, now); err != nil {
			s.log.Printf("replica: queue repo=%s: %v", run.InstanceID, err)
		}
	}
	s.replica.nudge()
}

// ReplicaReport is what the status endpoint and the UI show.
type ReplicaReport struct {
	Enabled bool   `json:"enabled"`
	Target  string `json:"target,omitempty"`
	Mirror  bool   `json:"mirror"`
	Keys    bool   `json:"include_keys"`
	// Healthy is false while the link is known to be down. It is the one field the UI
	// needs to decide between a green line and an alert.
	Healthy bool          `json:"healthy"`
	Link    ReplicaLink   `json:"link"`
	Counts  ReplicaCounts `json:"counts"`
	// Failing lists the items that are not getting through, newest first and capped.
	// A count alone does not tell an operator which backup is not off-site.
	Failing []*ReplicaItem `json:"failing,omitempty"`
}

// maxFailingReported caps the failing list. Past a handful the answer is "the link is
// down", which the health fields already say.
const maxFailingReported = 20

// replicaReport builds the status shown in the UI.
func (s *Server) replicaReport() (*ReplicaReport, error) {
	rep := &ReplicaReport{}
	if s.replica != nil {
		rep.Enabled = true
		rep.Target = s.replica.cfg.Addr()
		rep.Mirror = s.replica.cfg.Mirror
		rep.Keys = s.replica.cfg.IncludeKeys
	}
	link, err := s.store.ReplicaLinkState()
	if err != nil {
		return nil, err
	}
	counts, err := s.store.ReplicaQueueCounts()
	if err != nil {
		return nil, err
	}
	rep.Link, rep.Counts, rep.Healthy = link, counts, link.Healthy()
	if counts.Failed > 0 {
		items, err := s.store.ReplicaItems(0)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Status != ReplicaFailed {
				continue
			}
			rep.Failing = append(rep.Failing, it)
			if len(rep.Failing) == maxFailingReported {
				break
			}
		}
	}
	return rep, nil
}

// handleReplicaStatus reports the state of the off-site copy.
func (s *Server) handleReplicaStatus(w http.ResponseWriter, r *http.Request) {
	rep, err := s.replicaReport()
	if err != nil {
		s.log.Printf("replica status: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

// handleReplicaSync queues a full pass now, for an operator who has just repaired the
// link and does not want to wait for the next sweep.
func (s *Server) handleReplicaSync(w http.ResponseWriter, r *http.Request) {
	if s.replica == nil {
		writeError(w, http.StatusBadRequest, "off-site replication is not configured")
		return
	}
	go s.replica.sweep()
	writeJSON(w, map[string]any{"queued": true})
}

// handleReplicaRetry puts failed items back in line immediately, skipping their backoff.
func (s *Server) handleReplicaRetry(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.RetryReplicaFailed()
	if err != nil {
		s.log.Printf("replica retry: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if s.replica != nil {
		s.replica.nudge()
	}
	writeJSON(w, map[string]any{"retrying": n})
}

// replicaKeyFiles collects the key material to replicate from a loaded configuration.
// Reading it from the config rather than from a fixed directory means a rotation that
// introduces a new file cannot quietly stop being copied.
func ReplicaKeyFiles(cfg *config.Config) []string {
	var out []string
	add := func(paths ...string) {
		for _, p := range paths {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
	}
	add(cfg.TLS.CACert, cfg.TLS.Cert, cfg.TLS.Key)
	add(cfg.Signing.Key)
	add(cfg.Signing.PreviousKeys...)
	add(cfg.Secrets.MasterKey)
	add(cfg.Secrets.PreviousKeys...)
	add(cfg.Bootstrap.CAKey, cfg.Bootstrap.CACert)
	// The same file can be named by two settings (the trust bundle and the signing CA
	// are often one), and rsync would then transfer it twice.
	sort.Strings(out)
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i > 0 && s == prev {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// copyFileMode copies a small file, replacing the destination.
func copyFileMode(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}
