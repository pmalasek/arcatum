package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"
	"arcatum/web"
)

// Server wires the HTTP API to the store, scheduler and script catalog.
type Server struct {
	store             *Store
	sched             *Scheduler
	catalog           *Catalog
	log               *log.Logger
	signer            crypto.Signer
	requireClientCert bool
	// ca signs enrollment requests. Nil when the server has no CA key, in which case
	// approving a runner is not possible and says so.
	ca *crypto.CA
	// serverCertNotAfter is when this server's own certificate expires. Zero when mTLS
	// is off.
	serverCertNotAfter time.Time
	// serverCertIssuer is the authority that issued this server's certificate, needed to
	// spot a CA rotation performed in the wrong order.
	serverCertIssuer string
	// rotation holds the trust material published to runners, and what the server
	// currently signs certificates with.
	rotation RotationOptions
	// dist describes the published runner builds, for auto-update.
	dist *distCache
	// storage holds the last measurement of the backup directory, so the period view can
	// report disk usage without walking it inside a request (stats.go).
	storage *storageCache
	// bootstrapListen is the plain-HTTP listener install.sh is served from. Empty when
	// there is none.
	bootstrapListen string
	// web configures the plain-HTTP web listener's sessions (see users.go).
	web WebOptions
	// retention is how long finished runs' logs are kept (see retention.go).
	retention RetentionOptions
	// logins throttles failed password logins.
	logins *loginLimiter
	// configPath is the server.toml this process was started with. A configuration export
	// carries a copy of it for reference (config_archive.go); empty means none was found.
	configPath string
	// replica drives the off-site copy (replica.go). Nil when [replica] is not
	// configured, which every call site has to tolerate: replication is an addition to
	// the backups, never a precondition for them.
	replica *replicator
}

// Options carries the security wiring. Both fields are empty/false in development
// mode, where the server speaks plain HTTP and cannot identify its callers.
type Options struct {
	// Signer signs every dispatch so runners can prove a job came from Arcatum.
	Signer crypto.Signer
	// RequireClientCert enables certificate-based identity and role checks.
	RequireClientCert bool
	// CA signs enrollment requests from newly installed runners.
	CA *crypto.CA
	// ServerCertNotAfter is this server's certificate expiry, surfaced in the UI so it
	// does not lapse unnoticed.
	ServerCertNotAfter time.Time
	// ServerCertIssuer is the authority that issued this server's certificate.
	ServerCertIssuer string
	// Rotation is the trust material runners fetch, and the authority certificates are
	// issued under. See rotate.go.
	Rotation RotationOptions
	// DistDir holds the published runner binaries and their VERSION file. Empty disables
	// auto-update entirely.
	DistDir string
	// BootstrapListen is the address of the plain-HTTP bootstrap listener, e.g.
	// "0.0.0.0:80". Only its port is used, to tell the web UI where install.sh lives.
	// Empty means no bootstrap listener, and no installable runner.
	BootstrapListen string
	// Web configures password login on the plain-HTTP web listener (see users.go).
	Web WebOptions
	// Retention bounds how long finished runs' logs are kept. Backup payloads are never
	// removed by it (see retention.go).
	Retention RetentionOptions
	// ConfigPath is the server.toml in use, included in a configuration export so the
	// archive records how the server it came from was set up. Empty leaves it out.
	ConfigPath string
	// Replica configures the off-site copy. Leaving Enabled false switches it off, and
	// nothing else in the server changes (see replica.go).
	Replica ReplicaOptions
}

// New builds a Server over an open Store: loads the script catalog and starts tracking
// every schedule currently in the database.
func New(store *Store, scriptsDir string, loc *time.Location, logger *log.Logger, opts Options) (*Server, error) {
	cat, err := LoadCatalog(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	instances, err := store.Instances()
	if err != nil {
		return nil, fmt.Errorf("load instances: %w", err)
	}
	known := make(map[string]bool, len(instances))
	for _, in := range instances {
		if _, ok := cat.Get(in.Script); !ok {
			return nil, fmt.Errorf("instance %q references unknown script %q", in.ID, in.Script)
		}
		known[in.ID] = true
	}
	schedules, err := store.Schedules()
	if err != nil {
		return nil, fmt.Errorf("load schedules: %w", err)
	}
	sched := NewScheduler(loc)
	now := time.Now()
	for _, sc := range schedules {
		// A schedule whose instance is gone is a leftover, not a reason to refuse to start:
		// every other backup would stop over one orphaned row.
		if !known[sc.InstanceID] {
			logger.Printf("schedule %s: unknown instance %q, ignoring", sc.ID, sc.InstanceID)
			continue
		}
		if err := sched.TrackSchedule(sc, now); err != nil {
			return nil, fmt.Errorf("schedule %s (instance %q): %w", sc.ID, sc.InstanceID, err)
		}
	}
	srv := &Server{
		store:              store,
		sched:              sched,
		catalog:            cat,
		log:                logger,
		signer:             opts.Signer,
		requireClientCert:  opts.RequireClientCert,
		ca:                 opts.CA,
		serverCertNotAfter: opts.ServerCertNotAfter,
		serverCertIssuer:   opts.ServerCertIssuer,
		rotation:           opts.Rotation,
		dist:               &distCache{dir: opts.DistDir},
		storage:            &storageCache{},
		bootstrapListen:    opts.BootstrapListen,
		web:                opts.Web,
		retention:          opts.Retention,
		logins:             newLoginLimiter(),
		configPath:         opts.ConfigPath,
	}
	if opts.Replica.Enabled {
		timeout, sweep, probe, err := opts.Replica.Durations()
		if err != nil {
			return nil, err
		}
		srv.replica = &replicator{
			srv: srv, cfg: opts.Replica.Replica, keyFiles: opts.Replica.KeyFiles,
			timeout: timeout, sweepEvery: sweep, probeEvery: probe,
			wake: make(chan struct{}, 1),
		}
	}
	return srv, nil
}

// Arcatum listens on two ports, because the two kinds of caller are not alike:
//
//   - Handler (server.listen, mTLS): runners. A machine is installed once, gets a
//     certificate, and is refused during the TLS handshake if it has none — which is
//     exactly what you want from a host that pushes backups. The operator API is served
//     here too, for calling it with an admin certificate from a shell.
//   - WebHandler (web.listen, plain HTTP): people. The same operator API plus the web UI,
//     authenticated with a username and a password (users.go). No certificate to export
//     into every browser and no certificate to silently expire.
//
// Both listeners reach the same handlers; only the guard in front differs. The
// registration below is therefore shared, with the guard passed in.

// guard wraps a handler with the authentication of the listener it is registered on.
type guard func(http.HandlerFunc) http.HandlerFunc

// Handler returns the router for the mTLS listener: everything runners need, plus the
// operator API for an admin certificate (see auth.go).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// --- runners: identified by their certificate ---
	mux.HandleFunc("POST /api/v1/checkin", s.handleCheckin)
	mux.HandleFunc("POST /api/v1/runs/updates", s.handleUpdates)
	// The backup payload of a streaming job, raw and in one request (rundata.go).
	mux.HandleFunc("POST /api/v1/runs/{id}/data", s.handleUploadRunData)
	// Polled by the runner while a job runs: has an operator asked it to stop? (cancel.go)
	mux.HandleFunc("GET /api/v1/runs/{id}/cancel", s.handleRunCancelState)
	// Renewal is authenticated by the runner's current certificate, not by an operator.
	mux.HandleFunc("POST /api/v1/renew", s.handleRenew)
	// Trust material: runners fetch it every check-in so a rotation propagates by itself.
	mux.HandleFunc("GET /api/v1/trust", s.handleTrustBundle)
	// Auto-update: the manifest is signed, and binaries are served only over mTLS.
	mux.HandleFunc("GET /api/v1/update", s.handleUpdateManifest)
	mux.HandleFunc("GET /api/v1/update/{name}", s.handleUpdateDownload)
	// restic's REST backend: runners push file backups straight into the server's
	// repository for their own instances. Authorization is per repository (restic.go).
	mux.HandleFunc("/restic/", s.handleRestic)

	// --- operators: identified by an admin certificate ---
	s.registerOperatorRoutes(mux, s.adminOnly, s.adminOnly)
	// The root is the text status page here: the browser UI lives on the web listener,
	// where logging in with a password is possible. A bare "GET /" would also be a
	// catch-all and conflict with "/restic/", so it is matched exactly.
	mux.HandleFunc("GET /{$}", s.adminOnly(s.handleIndex))
	return mux
}

// WebHandler returns the router for the plain-HTTP web listener: the embedded UI,
// password login, account management and the operator API behind a session cookie.
func (s *Server) WebHandler() http.Handler {
	mux := http.NewServeMux()

	// Login is the one endpoint that cannot require a session.
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)
	// Changing your own password needs only a session, whatever the role.
	mux.HandleFunc("POST /api/v1/password", s.webRead(s.handleChangeOwnPassword))
	// Accounts: who may reach the system at all is an administrator's business.
	mux.HandleFunc("GET /api/v1/users", s.webAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/v1/users", s.webAdmin(s.handleCreateUser))
	mux.HandleFunc("PUT /api/v1/users/{name}", s.webAdmin(s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/v1/users/{name}", s.webAdmin(s.handleDeleteUser))

	// The operator API, with a viewer allowed to read and an admin allowed to change.
	s.registerOperatorRoutes(mux, s.webRead, s.webAdmin)

	// The UI itself is served without a session: it is the page that *asks* for the
	// login, and the assets carry no data of their own. Everything it then displays goes
	// through the API above.
	ui := http.FileServerFS(web.FS()).ServeHTTP
	mux.HandleFunc("GET /{$}", ui)
	for _, asset := range []string{"app.js", "style.css", "index.html"} {
		mux.HandleFunc("GET /"+asset, ui)
	}
	return mux
}

// registerOperatorRoutes adds the API an operator drives, with read wrapping the
// endpoints that only look and write wrapping those that change something. Splitting
// the two is what makes the viewer role possible on the web listener; on the mTLS
// listener both are the same admin-certificate check.
func (s *Server) registerOperatorRoutes(mux *http.ServeMux, read, write guard) {
	mux.HandleFunc("GET /api/v1/whoami", read(s.handleWhoAmI))
	mux.HandleFunc("GET /api/v1/instances", read(s.handleListInstances))
	mux.HandleFunc("POST /api/v1/instances", write(s.handleCreateInstance))
	mux.HandleFunc("PUT /api/v1/instances/{id}", write(s.handleUpdateInstance))
	mux.HandleFunc("DELETE /api/v1/instances/{id}", write(s.handleDeleteInstance))
	mux.HandleFunc("POST /api/v1/instances/{id}/run", write(s.handleTrigger))
	// Schedules: when an instance runs, kept apart from the instance itself because
	// pausing a nightly run and changing what gets backed up are different decisions.
	mux.HandleFunc("GET /api/v1/schedules", read(s.handleListSchedules))
	mux.HandleFunc("POST /api/v1/schedules", write(s.handleCreateSchedule))
	mux.HandleFunc("GET /api/v1/schedules/{id}", read(s.handleScheduleDetail))
	mux.HandleFunc("PUT /api/v1/schedules/{id}", write(s.handleUpdateSchedule))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", write(s.handleDeleteSchedule))
	mux.HandleFunc("GET /api/v1/instances/{id}/schedules", read(s.handleInstanceSchedules))
	// A task's own history. The flat list below stays for the shell; the UI reaches a run
	// through the task it belongs to.
	mux.HandleFunc("GET /api/v1/instances/{id}/runs", read(s.handleInstanceRuns))
	// The landing page, assembled in one request (dashboard.go).
	mux.HandleFunc("GET /api/v1/dashboard", read(s.handleDashboard))
	// The same history seen as a period rather than as a moment: how many backups ran in
	// the last N days, how big they were and how long they took (stats.go). Its own
	// endpoint because it is an aggregate over weeks, while the dashboard above is polled
	// every few seconds.
	mux.HandleFunc("GET /api/v1/stats", read(s.handleStats))
	mux.HandleFunc("GET /api/v1/scripts", read(s.handleListScripts))
	mux.HandleFunc("GET /api/v1/runs", read(s.handleListRuns))
	mux.HandleFunc("GET /api/v1/runs/{id}", read(s.handleRunDetail))
	mux.HandleFunc("GET /api/v1/runs/{id}/output", read(s.handleRunOutput))
	mux.HandleFunc("GET /api/v1/runs/{id}/tail", read(s.handleRunTail))
	mux.HandleFunc("GET /api/v1/runs/{id}/data", read(s.handleDownloadRunData))
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", write(s.handleCancelRun))
	mux.HandleFunc("GET /api/v1/runners", read(s.handleListRunners))
	mux.HandleFunc("GET /api/v1/install", read(s.handleInstallInfo))
	mux.HandleFunc("POST /api/v1/runners/{id}/approve", write(s.handleApproveRunner))
	mux.HandleFunc("POST /api/v1/runners/{id}/reject", write(s.handleRejectRunner))
	mux.HandleFunc("POST /api/v1/runners/{id}/revoke", write(s.handleRevokeRunner))
	mux.HandleFunc("POST /api/v1/runners/revoke-all", write(s.handleRevokeAllRunners))
	mux.HandleFunc("GET /api/v1/rotation", read(s.handleRotationStatus))
	mux.HandleFunc("POST /api/v1/secrets/rekey", write(s.handleRekeySecrets))
	// Off-site copy: its health and backlog are something a viewer needs to see, since a
	// replica that has quietly stopped working is the failure this whole subsystem
	// exists to make impossible. Forcing a pass changes what the server does, so it is
	// behind the write guard.
	mux.HandleFunc("GET /api/v1/replica", read(s.handleReplicaStatus))
	mux.HandleFunc("POST /api/v1/replica/sync", write(s.handleReplicaSync))
	mux.HandleFunc("POST /api/v1/replica/retry", write(s.handleReplicaRetry))
	// Administration: the server's own configuration in and out, and emptying it of
	// collected data. All four are admin-only — the export carries password verifiers,
	// and the other three destroy something. `write` is the admin guard on both
	// listeners, which is why the export is behind it despite being a GET.
	mux.HandleFunc("GET /api/v1/config/export", write(s.handleExportConfig))
	mux.HandleFunc("POST /api/v1/config/import", write(s.handleImportConfig))
	mux.HandleFunc("GET /api/v1/reset", write(s.handleResetPreview))
	mux.HandleFunc("POST /api/v1/reset", write(s.handleReset))
	mux.HandleFunc("GET /api/v1/instances/{id}/repo", read(s.handleRepoInfo))
	// Stored dumps of a streaming instance — the restore view's equivalent of snapshots.
	mux.HandleFunc("GET /api/v1/instances/{id}/dumps", read(s.handleListDumps))
	// Restore: browse the repository the server already holds and pull files back out.
	mux.HandleFunc("GET /api/v1/instances/{id}/snapshots", read(s.handleSnapshots))
	mux.HandleFunc("GET /api/v1/instances/{id}/snapshots/{snapshot}/ls", read(s.handleSnapshotLS))
	mux.HandleFunc("GET /api/v1/instances/{id}/snapshots/{snapshot}/download", read(s.handleRestoreDownload))
	// Plain-text status, handy from a shell where the browser UI is not.
	mux.HandleFunc("GET /status", read(s.handleIndex))
}

// handleCheckin registers the runner and returns the jobs due for it.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	var req proto.CheckinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// A rejected runner is refused even though it still holds a valid certificate, so
	// rejecting one in the UI takes effect without waiting for revocation.
	runnerID, err := s.activeRunnerIdentity(r, req.RunnerID)
	if err != nil {
		s.log.Printf("checkin denied: %v", err)
		s.denyRunner(w, err)
		return
	}
	req.RunnerID = runnerID // the certificate is authoritative

	now := time.Now()
	// Record the certificate's expiry and issuer, taken from the live connection: expiry
	// so it does not lapse unnoticed, issuer so a CA rotation can be tracked to
	// completion. Both are then known for hand-issued certificates too.
	var certNotAfter time.Time
	var certIssuer string
	if cert := peerCert(r); cert != nil {
		certNotAfter = cert.NotAfter
		certIssuer = cert.Issuer.CommonName
	}
	if err := s.store.RecordCheckin(req, certNotAfter, now, certIssuer); err != nil {
		s.log.Printf("checkin: record runner %q: %v", runnerID, err)
	}

	instances, err := s.store.InstancesForRunner(runnerID)
	if err != nil {
		s.log.Printf("checkin: instances for %q: %v", runnerID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var due []proto.JobDispatch
	for _, in := range instances {
		dueSchedules, manual := s.sched.DueFor(in.ID, now)
		if len(dueSchedules) == 0 && !manual {
			continue
		}
		// One run per instance per check-in. Two schedules landing in the same minute
		// describe the same work, and dispatching both would put two processes into one
		// repository — the very thing a single run per instance exists to prevent. The run
		// is attributed to the schedule that came due first; the others are advanced all
		// the same, so none of them fires again a moment later.
		scheduleID := ""
		if len(dueSchedules) > 0 {
			scheduleID = dueSchedules[0]
		}
		d, err := s.buildDispatch(in, scheduleID)
		if err != nil {
			s.log.Printf("checkin: instance %q: %v", in.ID, err)
			continue
		}
		for _, id := range dueSchedules {
			s.sched.MarkDispatched(id, now)
		}
		if manual {
			s.sched.ClearManual(in.ID)
		}
		due = append(due, d)
		s.log.Printf("dispatch: instance=%s run=%s schedule=%s -> runner=%s",
			in.ID, d.RunID, orManual(scheduleID), runnerID)
	}
	writeJSON(w, proto.CheckinResponse{Due: due})
}

// orManual labels a dispatch in the log: a run with no schedule behind it was asked for
// by a person, and saying so is more useful than an empty field.
func orManual(scheduleID string) string {
	if scheduleID == "" {
		return "manual"
	}
	return scheduleID
}

// buildDispatch turns an instance into a JobDispatch and records a pending Run.
// scheduleID says which schedule brought the run about, empty for a manual one; the
// dispatch itself does not carry it, because the runner has no use for it and the
// signed bytes are a compatibility contract with every deployed runner.
//
// When a signer is configured the dispatch is signed, which is what lets the runner
// prove the job really came from Arcatum before executing anything.
func (s *Server) buildDispatch(in *Instance, scheduleID string) (proto.JobDispatch, error) {
	entry, ok := s.catalog.Get(in.Script)
	if !ok {
		return proto.JobDispatch{}, fmt.Errorf("unknown script %q", in.Script)
	}
	content, sha, err := entry.readArtifact()
	if err != nil {
		return proto.JobDispatch{}, err
	}
	timeoutSec := timeoutSeconds(in.Timeout, entry.Manifest.Timeout)
	// The run carries the timeout it was dispatched with, so the server can later decide
	// a silent run is never coming back rather than leaving it "running" forever.
	run, err := s.store.CreateRun(in, scheduleID, timeoutSec)
	if err != nil {
		return proto.JobDispatch{}, fmt.Errorf("create run: %w", err)
	}
	d := proto.JobDispatch{
		RunID:      run.ID,
		InstanceID: in.ID,
		Script:     in.Script,
		Type:       entry.Manifest.Type,
		Artifact: proto.Artifact{
			Filename: entry.Manifest.Entrypoint,
			SHA256:   sha,
			Content:  content,
		},
		Params:     in.Params,
		Secrets:    in.Secrets,
		TimeoutSec: timeoutSec,
		Capture:    effectiveCapture(entry.Manifest, in),
	}
	if s.signer != nil {
		sig, err := s.signer.Sign(d.SigningBytes())
		if err != nil {
			return proto.JobDispatch{}, fmt.Errorf("sign dispatch: %w", err)
		}
		d.Signature = sig
	}
	return d, nil
}

// handleUpdates consumes a runner's ndjson stream of RunUpdate for one run. A runner
// may only report on runs dispatched to it, so one host cannot overwrite another's
// results.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	runnerID, err := s.activeRunnerIdentity(r, "")
	if err != nil && s.requireClientCert {
		s.log.Printf("updates denied: %v", err)
		s.denyRunner(w, err)
		return
	}

	allowed := map[string]bool{} // run id -> owned by this runner (cached per request)
	dec := json.NewDecoder(r.Body)
	for {
		var u proto.RunUpdate
		if err := dec.Decode(&u); err != nil {
			break // EOF or malformed tail — stop consuming
		}
		if s.requireClientCert && !s.ownsRun(allowed, u.RunID, runnerID) {
			s.log.Printf("updates denied: runner %q does not own run %s", runnerID, u.RunID)
			continue
		}
		s.applyUpdate(u)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownsRun reports whether runnerID is the runner a run was dispatched to, caching
// the answer so a long output stream costs one lookup.
func (s *Server) ownsRun(cache map[string]bool, runID, runnerID string) bool {
	if ok, seen := cache[runID]; seen {
		return ok
	}
	run, err := s.store.Run(runID)
	ok := err == nil && run != nil && run.RunnerID == runnerID
	cache[runID] = ok
	return ok
}

func (s *Server) applyUpdate(u proto.RunUpdate) {
	var err error
	switch u.Kind {
	case proto.KindStarted:
		err = s.store.MarkRunStarted(u.RunID, time.Now())
		if err == nil {
			s.log.Printf("run=%s started", u.RunID)
		}
	case proto.KindOutput:
		// AppendOutput accounts the bytes itself, buffered: one database write per chunk
		// is what made a large run crawl.
		_, err = s.store.AppendOutput(u.RunID, u.Stream, u.Data)
	case proto.KindFinished:
		err = s.store.FinishRun(u.RunID, time.Now(), u.ExitCode, u.Error)
		if err == nil {
			s.log.Printf("run=%s finished exit=%d err=%q", u.RunID, u.ExitCode, u.Error)
			// Off the update stream: rotating dumps means deleting files, and the runner
			// is still holding this request open. Retention runs before the off-site
			// queue, in that order rather than in two goroutines, so a dump that this
			// very run rotated away is never queued only to be unpicked.
			go func(runID string) {
				s.pruneDumpsForRun(runID)
				s.enqueueRunForReplica(runID)
			}(u.RunID)
		}
	}
	if err != nil {
		s.log.Printf("run=%s update %s: %v", u.RunID, u.Kind, err)
	}
}

// handleTrigger marks an instance for an immediate run (web "run now").
//
// Whether the instance exists is the store's question, not the scheduler's: an instance
// with no schedules at all is perfectly legal and must still be runnable by hand.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	in, err := s.store.Instance(id)
	if err != nil {
		s.log.Printf("trigger: instance %q: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if in == nil {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}
	s.sched.Trigger(id)
	s.log.Printf("trigger: instance=%s queued for next check-in", id)
	writeJSON(w, map[string]string{"status": "queued", "instance": id})
}

// handleListInstances returns instances with their next scheduled run and how many
// schedules they have. The counts travel with the list so the table can say "2
// schedules" without a request per row.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.store.Instances()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	schedules, err := s.store.Schedules()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total := map[string]int{}
	enabled := map[string]int{}
	for _, sc := range schedules {
		total[sc.InstanceID]++
		if sc.Enabled {
			enabled[sc.InstanceID]++
		}
	}
	type item struct {
		*Instance
		NextRun          *time.Time `json:"next_run,omitempty"`
		Schedules        int        `json:"schedules"`
		SchedulesEnabled int        `json:"schedules_enabled"`
	}
	out := make([]item, 0, len(instances))
	for _, in := range instances {
		it := item{Instance: in.Redacted()} // never expose secret values over the API
		if next, ok := s.sched.NextRunForInstance(in.ID); ok {
			it.NextRun = &next
		}
		it.Schedules, it.SchedulesEnabled = total[in.ID], enabled[in.ID]
		out = append(out, it)
	}
	writeJSON(w, out)
}

// handleListRuns returns runs newest-first; ?limit=N caps the result.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.store.ListRuns(limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, runs)
}

// handleRunDetail returns one run.
func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Run(r.PathValue("id"))
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	writeJSON(w, run)
}

// tailResponse is one chunk of a run's output plus where to continue from.
type tailResponse struct {
	Data   string    `json:"data"`
	Offset int64     `json:"offset"`
	Status RunStatus `json:"status"`
	Done   bool      `json:"done"` // the run finished, so no more output is coming
}

// maxTailChunk caps one tail response, so a huge log cannot be pulled in one request.
const maxTailChunk = 256 * 1024

// handleRunTail returns output from ?offset= onwards. The web UI calls this repeatedly
// while a run is in progress, which is how the live tail works without websockets.
func (s *Server) handleRunTail(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}
	data, newOffset, err := s.store.ReadOutputFrom(runID, r.URL.Query().Get("stream"), offset, maxTailChunk)
	if err != nil {
		s.log.Printf("tail run=%s: %v", runID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The status was read *before* the output on purpose. If the run finishes in
	// between, this response still says "running", so the client polls once more and
	// cannot miss the final lines. Reading it afterwards could report "done" while
	// output written after our read was still unsent.
	writeJSON(w, tailResponse{
		Data:   string(data),
		Offset: newOffset,
		Status: run.Status,
		Done:   isTerminal(run.Status),
	})
}

// isTerminal reports whether a run has reached a final state.
func isTerminal(st RunStatus) bool {
	switch st {
	case StatusSuccess, StatusFailed, StatusError, StatusCancelled:
		return true
	}
	return false
}

// handleRunOutput serves a run's captured output (?stream=stdout|stderr). This backs
// inspecting a run while debugging a script.
func (s *Server) handleRunOutput(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(s.store.StreamPath(runID, r.URL.Query().Get("stream")))
	if os.IsNotExist(err) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return // no output captured (yet)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleListRunners returns known runners, most recently seen first.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.store.Runners()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, runners)
}

// handleIndex is a minimal text status page (real web UI later). Routed as "/{$}", so
// it only ever sees the root path.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "arcatum-server\n\nscripts: %v\n", s.catalog.Names())

	if runners, err := s.store.Runners(); err == nil {
		fmt.Fprintf(w, "\nrunners:\n")
		for _, rn := range runners {
			fmt.Fprintf(w, "  %-20s %s/%s  last_seen=%s\n", rn.ID, rn.OS, rn.Arch,
				rn.LastSeen.Format(time.RFC3339))
		}
	}
	if instances, err := s.store.Instances(); err == nil {
		fmt.Fprintf(w, "\ninstances:\n")
		for _, in := range instances {
			next := "-"
			if t, ok := s.sched.NextRunForInstance(in.ID); ok {
				next = t.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "  %-20s script=%-14s runner=%-16s next_run=%s\n",
				in.ID, in.Script, in.RunnerID, next)
		}
	}
	if runs, err := s.store.ListRuns(20); err == nil {
		fmt.Fprintf(w, "\nruns (newest first):\n")
		for _, run := range runs {
			fmt.Fprintf(w, "  %-8s %-8s instance=%-16s exit=%-3d bytes=%d\n",
				run.ID, run.Status, run.InstanceID, run.ExitCode, run.Bytes)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with JSON. A rejected instance is rejected for a reason — a missing
// password, an unknown script — and the UI can only show that reason if it arrives in the
// shape fetch() knows how to read.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// timeoutSeconds resolves an instance/manifest timeout string to seconds, default 1h.
func timeoutSeconds(instTimeout, manifestTimeout string) int {
	for _, s := range []string{instTimeout, manifestTimeout} {
		if s == "" {
			continue
		}
		if d, err := time.ParseDuration(s); err == nil {
			return int(d.Seconds())
		}
	}
	return int((time.Hour).Seconds())
}
