package server

import (
	"net/http"
	"time"
)

// The dashboard answers the question an operator actually opens Arcatum with: is
// anything running, did anything break overnight, and what is due next. It is one
// endpoint rather than four, because the UI polls it every few seconds and a landing
// page assembled client-side would cost four requests per open browser per poll.

// failureWindow is how far back "recent failures" looks. A night's backups plus the
// morning somebody notices them fits inside a day.
const failureWindow = 24 * time.Hour

// dashboardCounts is the strip of figures across the top.
type dashboardCounts struct {
	Instances         int `json:"instances"`
	Schedules         int `json:"schedules"`
	SchedulesDisabled int `json:"schedules_disabled"`
	Running           int `json:"running"`
	Failed24h         int `json:"failed_24h"`
	RunnersOffline    int `json:"runners_offline"`
}

type dashboardResponse struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Counts      dashboardCounts `json:"counts"`
	Running     []*Run          `json:"running"`
	Failures24h []*Run          `json:"failures_24h"`
	Upcoming    []Upcoming      `json:"upcoming"`
}

// handleDashboard returns the overview: what is in flight, what failed in the last day,
// and what runs next.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()

	running, err := s.store.UnfinishedRuns()
	if err != nil {
		s.log.Printf("dashboard: unfinished runs: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	failures, err := s.store.RecentFailures(now.Add(-failureWindow), 20)
	if err != nil {
		s.log.Printf("dashboard: recent failures: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	instances, err := s.store.Instances()
	if err != nil {
		s.log.Printf("dashboard: instances: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	total, disabled, err := s.store.CountSchedules()
	if err != nil {
		s.log.Printf("dashboard: schedules: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The upcoming list carries each schedule's label, which the scheduler does not hold —
	// it tracks timing, not names.
	upcoming := s.sched.Upcoming(10)
	if len(upcoming) > 0 {
		if schedules, err := s.store.Schedules(); err == nil {
			names := make(map[string]string, len(schedules))
			for _, sc := range schedules {
				names[sc.ID] = sc.Name
			}
			for i := range upcoming {
				upcoming[i].Name = names[upcoming[i].ScheduleID]
			}
		}
	}

	offline := 0
	if runners, err := s.store.Runners(); err == nil {
		for _, rn := range runners {
			if rn.Status == EnrollApproved && !rn.LastSeen.IsZero() && now.Sub(rn.LastSeen) > runnerOfflineAfter {
				offline++
			}
		}
	}

	writeJSON(w, dashboardResponse{
		GeneratedAt: now,
		Counts: dashboardCounts{
			Instances:         len(instances),
			Schedules:         total,
			SchedulesDisabled: disabled,
			Running:           len(running),
			Failed24h:         len(failures),
			RunnersOffline:    offline,
		},
		Running:     orEmptyRuns(running),
		Failures24h: orEmptyRuns(failures),
		Upcoming:    upcoming,
	})
}

// runnerOfflineAfter is how long a silent runner counts as offline on the dashboard. The
// poll interval is 30s by default, so a few missed check-ins is a real absence rather
// than a slow network.
const runnerOfflineAfter = 5 * time.Minute

// orEmptyRuns keeps the JSON an array rather than null, so the UI can iterate blindly.
func orEmptyRuns(runs []*Run) []*Run {
	if runs == nil {
		return []*Run{}
	}
	return runs
}

// RecentFailures returns runs that ended badly since `since`, newest first. Cancelled
// runs are left out: somebody stopped those on purpose, and a dashboard that reports
// deliberate acts as failures teaches operators to ignore it.
func (s *Store) RecentFailures(since time.Time, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+runCols+`
		FROM runs WHERE status IN (?, ?) AND ended_at >= ? ORDER BY id DESC LIMIT ?`,
		string(StatusFailed), string(StatusError), toMillis(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRunPerSchedule returns the most recent run of each schedule, keyed by schedule id.
// Manual runs are excluded — they belong to no schedule, and letting one stand in for a
// nightly backup's result would say the night went fine when it never ran.
func (s *Store) LastRunPerSchedule() (map[string]*Run, error) {
	return s.lastRunBy(`SELECT `+runCols+` FROM runs
		WHERE id IN (SELECT MAX(id) FROM runs WHERE schedule_id != '' GROUP BY schedule_id)`,
		func(r *Run) string { return r.ScheduleID })
}

// LastRunPerInstance returns the most recent run of each task, keyed by instance id.
func (s *Store) LastRunPerInstance() (map[string]*Run, error) {
	return s.lastRunBy(`SELECT `+runCols+` FROM runs
		WHERE id IN (SELECT MAX(id) FROM runs GROUP BY instance_id)`,
		func(r *Run) string { return r.InstanceID })
}

func (s *Store) lastRunBy(query string, key func(*Run) string) (map[string]*Run, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out[key(r)] = r
	}
	return out, rows.Err()
}
