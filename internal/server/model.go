// Package server implements arcatum-server: HTTP API, scheduler and an in-memory
// store. Persistence (SQLite) and mTLS/signing come later; this stage proves the
// protocol end-to-end over plain HTTP.
package server

import (
	"fmt"
	"time"

	"arcatum/pkg/schedule"
)

// Instance binds a script definition to one runner with concrete parameters and a
// schedule. In this stage instances are loaded from a JSON file; later they live in
// the DB and are managed via the web UI. Secrets are plaintext here — encryption at
// rest lands with the DB.
type Instance struct {
	ID       string            `json:"id"`
	Script   string            `json:"script"`    // manifest name
	RunnerID string            `json:"runner_id"` // exactly one target (N:1)
	Params   map[string]string `json:"params"`    // non-secret -> env vars
	Secrets  map[string]string `json:"secrets"`   // secret -> temp file on the runner
	Capture  string            `json:"capture"`   // "stream" | "local"
	Timeout  string            `json:"timeout"`   // overrides manifest default
	Schedule ScheduleJSON      `json:"schedule"`
}

// Redacted returns a copy safe to expose over the API or in logs: secret *names*
// are kept (so the UI can show which are set) but their values are masked. Secret
// values leave the server only inside a JobDispatch to the owning runner.
func (i *Instance) Redacted() *Instance {
	copyInst := *i
	if i.Secrets != nil {
		copyInst.Secrets = make(map[string]string, len(i.Secrets))
		for k := range i.Secrets {
			copyInst.Secrets[k] = "***"
		}
	}
	return &copyInst
}

// ScheduleJSON is the on-disk form of a schedule.
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
	StatusPending RunStatus = "pending"
	StatusRunning RunStatus = "running"
	StatusSuccess RunStatus = "success"
	StatusFailed  RunStatus = "failed" // non-zero exit
	StatusError   RunStatus = "error"  // runner-side error before/after exec
)

// Run records one execution of an instance.
type Run struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id"`
	RunnerID   string    `json:"runner_id"`
	Script     string    `json:"script"`
	Status     RunStatus `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Bytes      int64     `json:"bytes"` // output/data bytes received
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	Err        string    `json:"err,omitempty"`
}
