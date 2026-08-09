// Package server implements arcatum-server: HTTP API, scheduler and an in-memory
// store. Persistence (SQLite) and mTLS/signing come later; this stage proves the
// protocol end-to-end over plain HTTP.
package server

import (
	"fmt"
	"time"

	"arcatum/pkg/schedule"
)

// Instance binds a script definition to one runner with concrete parameters. It is the
// definition of a task — what to back up, with which script, on which host — and says
// nothing about when it runs: that is Schedule, of which an instance may have several.
// An instance with no schedule at all is legal and is run on demand.
type Instance struct {
	ID       string            `json:"id"`
	Script   string            `json:"script"`    // manifest name
	RunnerID string            `json:"runner_id"` // exactly one target (N:1)
	Params   map[string]string `json:"params"`    // non-secret -> env vars
	Secrets  map[string]string `json:"secrets"`   // secret -> temp file on the runner
	Capture  string            `json:"capture"`   // "" (follow the manifest) | "log" | "local"
	Timeout  string            `json:"timeout"`   // overrides manifest default
	// KeepLast and KeepDays bound how many backup dumps this instance keeps, for a script
	// that streams its payload (capture = "stream"). A dump is restored whole, so it is
	// rotated rather than deduplicated. The two are a union — "at least this many, and at
	// least this old" — and 0 for both keeps everything. See dumps.go.
	KeepLast int `json:"keep_last"`
	KeepDays int `json:"keep_days"`
}

// Redacted returns a copy safe to expose over the API or in logs: secret *names*
// are kept (so the UI can show which are set) but their values are masked. Secret
// values leave the server only inside a JobDispatch to the owning runner.
func (i *Instance) Redacted() *Instance {
	copyInst := *i
	if i.Secrets != nil {
		copyInst.Secrets = make(map[string]string, len(i.Secrets))
		for k := range i.Secrets {
			copyInst.Secrets[k] = redactedSecret
		}
	}
	return &copyInst
}

// Schedule is one "when" for one instance. An instance may have several: a nightly
// incremental and a full copy once a month are two schedules of the same task, not two
// tasks. Splitting them out of the instance is what makes that possible, and what lets a
// schedule be paused without touching the backup it belongs to.
type Schedule struct {
	ID         string `json:"id"` // "sch-<n>"
	InstanceID string `json:"instance_id"`
	Name       string `json:"name,omitempty"` // operator's label, e.g. "nightly"
	// The timing itself, flattened into the same JSON object.
	ScheduleJSON
	// Enabled false keeps the definition and stops it coming due. See scheduler.go.
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ScheduleJSON is the timing half of a schedule: frequency, time of day and which days
// it applies to. It is also the shape schedules had when they lived inside an instance,
// so it is what an older config archive or seed file is read back through.
type ScheduleJSON struct {
	Frequency string   `json:"frequency"` // daily | weekly | monthly
	Time      string   `json:"time"`      // HH:MM
	Weekdays  []string `json:"weekdays"`  // for weekly, e.g. ["mon","thu"]
	Day       int      `json:"day"`       // for monthly (1-28)
	Timezone  string   `json:"timezone"`  // falls back to the server default
}

// Spec converts the JSON schedule into a schedule.Spec, using defLoc when the
// instance doesn't set its own timezone.
func (s ScheduleJSON) Spec(defLoc *time.Location) (schedule.Spec, error) {
	loc := defLoc
	if s.Timezone != "" {
		l, err := time.LoadLocation(s.Timezone)
		if err != nil {
			return schedule.Spec{}, fmt.Errorf("schedule: invalid timezone %q: %w", s.Timezone, err)
		}
		loc = l
	}
	hour, min, err := schedule.ParseHHMM(s.Time)
	if err != nil {
		return schedule.Spec{}, err
	}
	var wds []time.Weekday
	for _, w := range s.Weekdays {
		wd, err := schedule.ParseWeekday(w)
		if err != nil {
			return schedule.Spec{}, err
		}
		wds = append(wds, wd)
	}
	return schedule.Spec{
		Frequency: schedule.Frequency(s.Frequency),
		Hour:      hour,
		Minute:    min,
		Weekdays:  wds,
		Day:       s.Day,
		Location:  loc,
	}, nil
}

// RunStatus is the lifecycle state of a single execution.
type RunStatus string

const (
	StatusPending   RunStatus = "pending"
	StatusRunning   RunStatus = "running"
	StatusSuccess   RunStatus = "success"
	StatusFailed    RunStatus = "failed"    // non-zero exit
	StatusError     RunStatus = "error"     // runner-side error before/after exec
	StatusCancelled RunStatus = "cancelled" // stopped on an operator's request
)

// Run records one execution of an instance.
type Run struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	RunnerID   string `json:"runner_id"`
	Script     string `json:"script"`
	// ScheduleID is the schedule that caused this run; empty means somebody pressed
	// "run now". Without it a task's history cannot say which of its schedules a run
	// came from, which is the whole point of having more than one.
	ScheduleID string    `json:"schedule_id,omitempty"`
	Status     RunStatus `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Bytes      int64     `json:"bytes"`      // log bytes received (stdout+stderr)
	DataBytes  int64     `json:"data_bytes"` // backup payload received (capture = "stream")
	// CreatedAt is when the run was dispatched, which is the only clock a run that never
	// started has (reaper.go).
	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Err       string    `json:"err,omitempty"`
	// TimeoutSec is what the run was dispatched with; 0 means the default applied.
	TimeoutSec int `json:"timeout_sec"`
	// CancelRequested is set once an operator has asked for the run to stop. The UI uses
	// it to show that a stop is on its way but the runner has not acted on it yet.
	CancelRequested bool `json:"cancel_requested"`
	// DataPruned says the backup payload has been rotated away by retention. DataBytes
	// still reports what the run produced, so the history stays honest.
	DataPruned bool `json:"data_pruned"`
	// ReplicaStatus is how far this run's data has got towards the off-site copy: the
	// state of its run directory, or of the repository a restic backup wrote into. Empty
	// means it was never queued — replication is off, or the run predates it — which the
	// UI shows as a dash rather than as a failure (replica.go).
	ReplicaStatus ReplicaStatus `json:"replica_status,omitempty"`
}
