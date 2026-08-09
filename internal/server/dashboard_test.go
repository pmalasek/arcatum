package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The dashboard is the landing page, so what it must not do is either hide a failure or
// invent one. The window and what counts as a failure are the whole substance of it.

func decodeDashboard(t *testing.T, srv *Server) *dashboardResponse {
	t.Helper()
	rec := apiCall(t, srv, http.MethodGet, "/api/v1/dashboard", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var d dashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode dashboard: %v (%s)", err, rec.Body.String())
	}
	return &d
}

// finishRunAt closes a run and backdates its end, so the 24-hour window can be tested
// without waiting a day.
func finishRunAt(t *testing.T, st *Store, runID string, exitCode int, endedAt time.Time) {
	t.Helper()
	if err := st.FinishRun(runID, endedAt, exitCode, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	n, err := parseRunID(runID)
	if err != nil {
		t.Fatalf("parseRunID: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE runs SET ended_at = ? WHERE id = ?`, toMillis(endedAt), n); err != nil {
		t.Fatalf("backdate run: %v", err)
	}
}

func TestDashboardFailureWindow(t *testing.T) {
	srv := scheduleAPIServer(t)
	inst, _ := srv.store.Instance("mysql-web01")
	now := time.Now()

	recent, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	finishRunAt(t, srv.store, recent.ID, 1, now.Add(-time.Hour))

	old, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	finishRunAt(t, srv.store, old.ID, 1, now.Add(-30*time.Hour))

	d := decodeDashboard(t, srv)
	if len(d.Failures24h) != 1 || d.Failures24h[0].ID != recent.ID {
		t.Fatalf("failures = %v, want only %s", d.Failures24h, recent.ID)
	}
	if d.Counts.Failed24h != 1 {
		t.Errorf("failed_24h = %d, want 1", d.Counts.Failed24h)
	}
}

// A run somebody stopped on purpose is not a fault to chase. A dashboard that reports
// deliberate acts as failures is one operators learn to ignore.
func TestDashboardIgnoresCancelledRuns(t *testing.T) {
	srv := scheduleAPIServer(t)
	inst, _ := srv.store.Instance("mysql-web01")

	run, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.RequestRunCancel(run.ID); err != nil {
		t.Fatalf("RequestRunCancel: %v", err)
	}
	finishRunAt(t, srv.store, run.ID, 143, time.Now().Add(-time.Minute))

	if got, err := srv.store.Run(run.ID); err != nil || got.Status != StatusCancelled {
		t.Fatalf("status = %v, %v; want cancelled", got.Status, err)
	}
	if d := decodeDashboard(t, srv); len(d.Failures24h) != 0 {
		t.Errorf("failures = %v, want a cancelled run left out", d.Failures24h)
	}
}

// What is in flight, and what is coming — the other two questions the page answers.
func TestDashboardRunningAndUpcoming(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))
	inst, _ := srv.store.Instance("mysql-web01")

	run, err := srv.store.CreateRun(inst, created.ID, 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.MarkRunStarted(run.ID, time.Now()); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}

	d := decodeDashboard(t, srv)
	if len(d.Running) != 1 || d.Running[0].ID != run.ID {
		t.Fatalf("running = %v, want %s", d.Running, run.ID)
	}
	if d.Counts.Running != 1 || d.Counts.Instances != 1 || d.Counts.Schedules != 1 {
		t.Errorf("counts = %+v, want one of each", d.Counts)
	}
	if len(d.Upcoming) != 1 || d.Upcoming[0].ScheduleID != created.ID {
		t.Fatalf("upcoming = %v, want the one schedule", d.Upcoming)
	}
	// The label comes from the store — the scheduler tracks timing, not names.
	if d.Upcoming[0].Name != "nightly" {
		t.Errorf("upcoming name = %q, want nightly", d.Upcoming[0].Name)
	}

	// A paused schedule is not coming, and says so by being absent.
	if rec := apiCall(t, srv, http.MethodPut, "/api/v1/schedules/"+created.ID,
		map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d (%s)", rec.Code, rec.Body.String())
	}
	d = decodeDashboard(t, srv)
	if len(d.Upcoming) != 0 {
		t.Errorf("upcoming = %v, want a paused schedule left out", d.Upcoming)
	}
	if d.Counts.SchedulesDisabled != 1 {
		t.Errorf("schedules_disabled = %d, want 1", d.Counts.SchedulesDisabled)
	}
}
