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
	// web configures the plain-HTTP web listener's sessions (see users.go).
	web WebOptions
	// logins throttles failed password logins.
	logins *loginLimiter
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
	// Web configures password login on the plain-HTTP web listener (see users.go).
	Web WebOptions
}

// New builds a Server over an open Store: loads the script catalog and starts
// tracking the schedule of every instance currently in the database.
func New(store *Store, scriptsDir string, loc *time.Location, logger *log.Logger, opts Options) (*Server, error) {
	cat, err := LoadCatalog(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	instances, err := store.Instances()
	if err != nil {
		return nil, fmt.Errorf("load instances: %w", err)
	}
	sched := NewScheduler(loc)
	now := time.Now()
	for _, in := range instances {
		if _, ok := cat.Get(in.Script); !ok {
			return nil, fmt.Errorf("instance %q references unknown script %q", in.ID, in.Script)
		}
		if err := sched.Track(in, now); err != nil {
			return nil, fmt.Errorf("instance %q schedule: %w", in.ID, err)
		}
	}
	return &Server{
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
		web:                opts.Web,
		logins:             newLoginLimiter(),
	}, nil
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
	mux.HandleFunc("GET /api/v1/scripts", read(s.handleListScripts))
	mux.HandleFunc("GET /api/v1/runs", read(s.handleListRuns))
	mux.HandleFunc("GET /api/v1/runs/{id}", read(s.handleRunDetail))
	mux.HandleFunc("GET /api/v1/runs/{id}/output", read(s.handleRunOutput))
	mux.HandleFunc("GET /api/v1/runs/{id}/tail", read(s.handleRunTail))
	mux.HandleFunc("GET /api/v1/runners", read(s.handleListRunners))
	mux.HandleFunc("POST /api/v1/runners/{id}/approve", write(s.handleApproveRunner))
	mux.HandleFunc("POST /api/v1/runners/{id}/reject", write(s.handleRejectRunner))
	mux.HandleFunc("POST /api/v1/runners/{id}/revoke", write(s.handleRevokeRunner))
	mux.HandleFunc("POST /api/v1/runners/revoke-all", write(s.handleRevokeAllRunners))
	mux.HandleFunc("GET /api/v1/rotation", read(s.handleRotationStatus))
	mux.HandleFunc("POST /api/v1/secrets/rekey", write(s.handleRekeySecrets))
	mux.HandleFunc("GET /api/v1/instances/{id}/repo", read(s.handleRepoInfo))
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
		if !s.sched.Due(in.ID, now) {
			continue
		}
		d, err := s.buildDispatch(in)
		if err != nil {
			s.log.Printf("checkin: instance %q: %v", in.ID, err)
			continue
		}
		s.sched.MarkDispatched(in.ID, now)
		due = append(due, d)
		s.log.Printf("dispatch: instance=%s run=%s -> runner=%s", in.ID, d.RunID, runnerID)
	}
	writeJSON(w, proto.CheckinResponse{Due: due})
}

// buildDispatch turns an instance into a JobDispatch and records a pending Run. When
// a signer is configured the dispatch is signed, which is what lets the runner prove
// the job really came from Arcatum before executing anything.
func (s *Server) buildDispatch(in *Instance) (proto.JobDispatch, error) {
	entry, ok := s.catalog.Get(in.Script)
	if !ok {
		return proto.JobDispatch{}, fmt.Errorf("unknown script %q", in.Script)
	}
	content, sha, err := entry.readArtifact()
	if err != nil {
		return proto.JobDispatch{}, err
	}
	run, err := s.store.CreateRun(in)
	if err != nil {
		return proto.JobDispatch{}, fmt.Errorf("create run: %w", err)
	}
	capture := in.Capture
	if capture == "" {
		capture = "stream"
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
		TimeoutSec: timeoutSeconds(in.Timeout, entry.Manifest.Timeout),
		Capture:    capture,
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
		var n int
		n, err = s.store.AppendOutput(u.RunID, u.Stream, u.Data)
		if err == nil {
			err = s.store.AddRunBytes(u.RunID, int64(n))
		}
	case proto.KindFinished:
		err = s.store.FinishRun(u.RunID, time.Now(), u.ExitCode, u.Error)
		if err == nil {
			s.log.Printf("run=%s finished exit=%d err=%q", u.RunID, u.ExitCode, u.Error)
		}
	}
	if err != nil {
		s.log.Printf("run=%s update %s: %v", u.RunID, u.Kind, err)
	}
}

// handleTrigger marks an instance for an immediate run (web "run now").
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.sched.Trigger(id) {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}
	s.log.Printf("trigger: instance=%s queued for next check-in", id)
	writeJSON(w, map[string]string{"status": "queued", "instance": id})
}

// handleListInstances returns instances with their next scheduled run.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.store.Instances()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	type item struct {
		*Instance
		NextRun *time.Time `json:"next_run,omitempty"`
	}
	out := make([]item, 0, len(instances))
	for _, in := range instances {
		it := item{Instance: in.Redacted()} // never expose secret values over the API
		if next, ok := s.sched.NextRun(in.ID); ok {
			it.NextRun = &next
		}
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
	case StatusSuccess, StatusFailed, StatusError:
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
			if t, ok := s.sched.NextRun(in.ID); ok {
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
