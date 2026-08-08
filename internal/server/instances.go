package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"arcatum/pkg/jobspec"
	"arcatum/pkg/proto"
)

// Managing instances through the API is what takes the plaintext seed file out of the
// picture: an instance created here has its secrets encrypted from the moment it is
// saved, and adding a backup no longer means editing JSON on the server and restarting.
//
// The values are validated against the script's manifest, which is what those parameter
// declarations were designed for — a missing password or a mistyped parameter name is
// caught when the instance is saved rather than when the backup runs at two in the
// morning.

// ErrInstanceExists means a create would overwrite an existing instance.
var ErrInstanceExists = errors.New("instance already exists")

// ErrInstanceNotFound means the instance is not in the database.
var ErrInstanceNotFound = errors.New("instance not found")

// SaveInstance writes an instance, either creating it or replacing an existing one.
func (s *Store) SaveInstance(in *Instance, mustBeNew bool) error {
	exists, err := s.instanceExists(in.ID)
	if err != nil {
		return err
	}
	if mustBeNew && exists {
		return fmt.Errorf("%q: %w", in.ID, ErrInstanceExists)
	}
	if !mustBeNew && !exists {
		return fmt.Errorf("%q: %w", in.ID, ErrInstanceNotFound)
	}

	params, err := json.Marshal(orEmptyMap(in.Params))
	if err != nil {
		return err
	}
	sealed, err := s.sealSecrets(in.ID, orEmptyMap(in.Secrets))
	if err != nil {
		return err
	}
	secrets, err := json.Marshal(orEmptyMap(sealed))
	if err != nil {
		return err
	}
	sched, err := json.Marshal(in.Schedule)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO instances (id, script, runner_id, params, secrets, capture, timeout, schedule,
		  keep_last, keep_days)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  script=excluded.script, runner_id=excluded.runner_id, params=excluded.params,
		  secrets=excluded.secrets, capture=excluded.capture, timeout=excluded.timeout,
		  schedule=excluded.schedule, keep_last=excluded.keep_last, keep_days=excluded.keep_days`,
		in.ID, in.Script, in.RunnerID, string(params), string(secrets),
		in.Capture, in.Timeout, string(sched), in.KeepLast, in.KeepDays)
	return err
}

// DeleteInstance removes an instance. Its backup repository is deliberately left on
// disk: deleting a configuration entry must not throw away the backups it produced.
func (s *Store) DeleteInstance(id string) error {
	res, err := s.db.Exec(`DELETE FROM instances WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", id, ErrInstanceNotFound)
	}
	return nil
}

func (s *Store) instanceExists(id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM instances WHERE id = ?`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// --- HTTP ---------------------------------------------------------------------

// ScriptInfo describes a script and the parameters it accepts, so the web UI can render
// a form instead of asking an operator to hand-write JSON.
type ScriptInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Timeout    string `json:"timeout,omitempty"`
	// Capture tells the UI whether this script's stdout is the backup payload, which is
	// what decides whether an instance of it has dumps to rotate (see dumps.go).
	Capture string      `json:"capture"`
	Params  []ParamInfo `json:"params"`
}

// ParamInfo is one declared parameter.
type ParamInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Secret   bool   `json:"secret"`
	Default  string `json:"default,omitempty"`
}

// handleListScripts returns the script catalogue with parameter declarations.
func (s *Server) handleListScripts(w http.ResponseWriter, r *http.Request) {
	out := []ScriptInfo{}
	for _, name := range s.catalog.Names() {
		entry, ok := s.catalog.Get(name)
		if !ok {
			continue
		}
		m := entry.Manifest
		info := ScriptInfo{
			Name:       m.Name,
			Type:       string(m.Type),
			Entrypoint: m.Entrypoint,
			Timeout:    m.Timeout,
			Capture:    effectiveManifestCapture(m),
			Params:     []ParamInfo{},
		}
		for _, p := range m.Params {
			info.Params = append(info.Params, ParamInfo{
				Name: p.Name, Type: p.Type, Required: p.Required,
				Secret: p.Secret, Default: p.Default,
			})
		}
		out = append(out, info)
	}
	writeJSON(w, out)
}

// instancePayload is what the API accepts for an instance.
type instancePayload struct {
	ID       string `json:"id"`
	Script   string `json:"script"`
	RunnerID string `json:"runner_id"`
	// CopyFrom names an instance to take secrets from when creating this one. Secrets
	// leave the server only masked, so a copy made from what the API hands out would
	// otherwise mean retyping every password the original already has.
	CopyFrom string            `json:"copy_from"`
	Params   map[string]string `json:"params"`
	Secrets  map[string]string `json:"secrets"`
	Capture  string            `json:"capture"`
	Timeout  string            `json:"timeout"`
	Schedule ScheduleJSON      `json:"schedule"`
	KeepLast int               `json:"keep_last"`
	KeepDays int               `json:"keep_days"`
}

// handleCreateInstance adds a new instance (admin only). With copy_from it starts from
// another instance: everything visible the caller sends itself, and the secrets it can
// only send back masked are taken from the named source.
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var p instancePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	var source *Instance
	if p.CopyFrom != "" {
		var err error
		if source, err = s.store.Instance(p.CopyFrom); err != nil {
			s.log.Printf("instance copy source %q: %v", p.CopyFrom, err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if source == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source instance %q", p.CopyFrom))
			return
		}
	}
	in, err := s.instanceFromPayload(p, source)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SaveInstance(in, true); err != nil {
		s.instanceStoreError(w, err)
		return
	}
	if err := s.sched.Track(in, time.Now()); err != nil {
		s.log.Printf("instance %q: schedule: %v", in.ID, err)
	}
	if source != nil {
		s.log.Printf("instance %q created as a copy of %q (script=%s runner=%s)",
			in.ID, source.ID, in.Script, in.RunnerID)
	} else {
		s.log.Printf("instance %q created (script=%s runner=%s)", in.ID, in.Script, in.RunnerID)
	}
	writeJSONStatus(w, http.StatusCreated, in.Redacted())
}

// handleUpdateInstance replaces an existing instance (admin only).
func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.Instance(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "unknown instance")
		return
	}
	var p instancePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	p.ID = id // the path decides which instance this is
	in, err := s.instanceFromPayload(p, existing)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SaveInstance(in, false); err != nil {
		s.instanceStoreError(w, err)
		return
	}
	// Re-tracking recomputes the next run, so a changed schedule takes effect at once
	// rather than at the next restart.
	if err := s.sched.Track(in, time.Now()); err != nil {
		s.log.Printf("instance %q: schedule: %v", in.ID, err)
	}
	s.log.Printf("instance %q updated", in.ID)
	writeJSON(w, in.Redacted())
}

// handleDeleteInstance removes an instance (admin only).
func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteInstance(id); err != nil {
		s.instanceStoreError(w, err)
		return
	}
	s.sched.Untrack(id)
	s.log.Printf("instance %q deleted (its backup repository is kept)", id)
	writeJSON(w, map[string]string{"status": "deleted", "instance": id})
}

// instanceFromPayload validates a payload and turns it into an instance.
//
// When updating, a secret left out — or sent back as the mask the API hands out — keeps
// its stored value. Without that, loading a form and saving it would replace every
// password with the string "***". A copy resolves its masked secrets the same way, only
// against the instance it is copied from (existing is then that source).
func (s *Server) instanceFromPayload(p instancePayload, existing *Instance) (*Instance, error) {
	if err := validateInstanceID(p.ID); err != nil {
		return nil, err
	}
	if p.RunnerID == "" {
		return nil, fmt.Errorf("runner_id is required")
	}
	entry, ok := s.catalog.Get(p.Script)
	if !ok {
		return nil, fmt.Errorf("unknown script %q", p.Script)
	}
	if p.Timeout != "" {
		if _, err := time.ParseDuration(p.Timeout); err != nil {
			return nil, fmt.Errorf("timeout %q: %w", p.Timeout, err)
		}
	}
	// Retention of backup dumps. Negative would silently mean "keep everything", which is
	// the opposite of what somebody typing -1 expects.
	if p.KeepLast < 0 || p.KeepDays < 0 {
		return nil, fmt.Errorf("keep_last and keep_days must not be negative")
	}
	// What stdout is comes from the script's manifest; this only lets an instance opt
	// out of streaming (see effectiveCapture).
	switch p.Capture {
	case "", proto.CaptureLog, proto.CaptureStream, proto.CaptureLocal:
	default:
		return nil, fmt.Errorf("capture must be %q, %q or %q, got %q",
			proto.CaptureLog, proto.CaptureStream, proto.CaptureLocal, p.Capture)
	}

	secrets := map[string]string{}
	for name, value := range p.Secrets {
		if value == redactedSecret || value == "" {
			if existing != nil {
				if old, ok := existing.Secrets[name]; ok {
					secrets[name] = old
					continue
				}
			}
			// Nothing stored to keep, so treat it as not provided and let validation
			// report it if the script requires it.
			continue
		}
		secrets[name] = value
	}
	// Secrets the payload does not mention at all are kept too, so a partial update does
	// not quietly drop a password. A copy takes only the secrets it asked for: the source
	// may be running a different script, whose parameters the new one knows nothing about.
	if existing != nil && existing.ID == p.ID {
		for name, old := range existing.Secrets {
			if _, mentioned := p.Secrets[name]; !mentioned {
				secrets[name] = old
			}
		}
	}

	in := &Instance{
		ID:       p.ID,
		Script:   p.Script,
		RunnerID: p.RunnerID,
		Params:   orEmptyMap(p.Params),
		Secrets:  secrets,
		Capture:  p.Capture,
		Timeout:  p.Timeout,
		Schedule: p.Schedule,
		KeepLast: p.KeepLast,
		KeepDays: p.KeepDays,
	}
	// A dispatch carries the stored params and secrets verbatim, so a declared default has
	// to be materialised here or it would never reach the runner: validation would pass on
	// the strength of the manifest and the job would then fail for a missing value.
	applyDefaults(entry.Manifest, in.Params, in.Secrets)
	// The manifest is the contract: check the values against it before storing.
	if err := entry.Manifest.ValidateParams(in.Params, in.Secrets); err != nil {
		return nil, err
	}
	// A schedule that cannot be parsed would leave the instance never running.
	if _, err := in.Schedule.Spec(s.sched.loc); err != nil {
		return nil, err
	}
	return in, nil
}

// applyDefaults fills in the values the manifest declares a default for, leaving anything
// the operator actually supplied alone. A blank field counts as not supplied: the form
// sends empty strings for fields left untouched, and treating those as a deliberate empty
// value would defeat the point of having a default.
func applyDefaults(m *jobspec.Manifest, params, secrets map[string]string) {
	for _, p := range m.Params {
		if p.Default == "" {
			continue
		}
		target := params
		if p.Secret {
			target = secrets
		}
		if strings.TrimSpace(target[p.Name]) == "" {
			target[p.Name] = p.Default
		}
	}
}

// redactedSecret is the placeholder the API returns instead of a secret value.
const redactedSecret = "***"

func (s *Server) instanceStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInstanceExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInstanceNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.log.Printf("instance store: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
