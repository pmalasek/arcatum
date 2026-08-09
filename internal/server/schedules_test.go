package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The schedule API is where "when does this run" is decided now. What these cover is the
// validation an operator relies on to find out at save time rather than at two in the
// morning, and that changes take effect without a restart.

// scheduleAPIServer is an instance server with one saved instance to hang schedules off.
func scheduleAPIServer(t *testing.T) *Server {
	t.Helper()
	srv := instanceAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance()); rec.Code != http.StatusCreated {
		t.Fatalf("create instance = %d (%s)", rec.Code, rec.Body.String())
	}
	return srv
}

func validSchedule() map[string]any {
	return map[string]any{
		"instance_id": "mysql-web01",
		"name":        "nightly",
		"frequency":   "daily",
		"time":        "02:30",
	}
}

func decodeSchedule(t *testing.T, rec *httptest.ResponseRecorder) *Schedule {
	t.Helper()
	var sc Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &sc); err != nil {
		t.Fatalf("decode schedule: %v (%s)", err, rec.Body.String())
	}
	return &sc
}

// One task, two schedules — the thing that was impossible while timing lived inside the
// instance, and the reason for the whole split.
func TestCreateSeveralSchedulesForOneInstance(t *testing.T) {
	srv := scheduleAPIServer(t)

	rec := apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	nightly := decodeSchedule(t, rec)

	monthly := validSchedule()
	monthly["name"] = "monthly full"
	monthly["frequency"] = "monthly"
	monthly["day"] = 1
	monthly["time"] = "23:00"
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/schedules", monthly); rec.Code != http.StatusCreated {
		t.Fatalf("create monthly = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	list, err := srv.store.SchedulesForInstance("mysql-web01")
	if err != nil || len(list) != 2 {
		t.Fatalf("SchedulesForInstance = %v, %v; want two", list, err)
	}
	// Both are tracked straight away, without a restart.
	if _, ok := srv.sched.NextRun(nightly.ID); !ok {
		t.Error("a new schedule must be tracked immediately")
	}
	if _, ok := srv.sched.NextRunForInstance("mysql-web01"); !ok {
		t.Error("the instance must report a next run once it has schedules")
	}
}

// Everything here is a state that would otherwise surface as a task that quietly never
// runs — which is the failure this system exists to prevent.
func TestCreateScheduleValidation(t *testing.T) {
	srv := scheduleAPIServer(t)

	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"unknown instance", func(m map[string]any) { m["instance_id"] = "nope" }, "nope"},
		{"no instance", func(m map[string]any) { m["instance_id"] = "" }, "instance_id"},
		// Accepted before the split: Spec built a Frequency("hourly") happily and the
		// failure only appeared later, inside Next, as an instance that never ran.
		{"unknown frequency", func(m map[string]any) { m["frequency"] = "hourly" }, "frequency"},
		{"bad time", func(m map[string]any) { m["time"] = "25:00" }, "time"},
		{"monthly without a day", func(m map[string]any) { m["frequency"] = "monthly" }, "day"},
		{"monthly past the 28th", func(m map[string]any) {
			m["frequency"] = "monthly"
			m["day"] = 31
		}, "day"},
		{"bad timezone", func(m map[string]any) { m["timezone"] = "Mars/Olympus" }, "timezone"},
		{"bad weekday", func(m map[string]any) {
			m["frequency"] = "weekly"
			m["weekdays"] = []string{"someday"}
		}, "someday"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := validSchedule()
			tc.mutate(payload)
			rec := apiCall(t, srv, http.MethodPost, "/api/v1/schedules", payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("error %q should mention %q", rec.Body.String(), tc.want)
			}
		})
	}
	if list, _ := srv.store.SchedulesForInstance("mysql-web01"); len(list) != 0 {
		t.Errorf("%d schedule(s) were stored despite being refused", len(list))
	}
}

// A changed time has to apply at once. Waiting for a restart is how a schedule an
// operator believes they fixed runs at the old hour that night.
func TestUpdateScheduleRetracks(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))
	before, _ := srv.sched.NextRun(created.ID)

	payload := validSchedule()
	payload["time"] = "23:45"
	rec := apiCall(t, srv, http.MethodPut, "/api/v1/schedules/"+created.ID, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	after, ok := srv.sched.NextRun(created.ID)
	if !ok {
		t.Fatal("the schedule stopped being tracked after an update")
	}
	if after.Equal(before) || after.Hour() != 23 || after.Minute() != 45 {
		t.Errorf("next run = %s, want it moved to 23:45", after)
	}
}

// Pausing is a one-field request and must not disturb the timing: that is the difference
// between pausing and deleting-and-retyping, which is how a schedule comes back subtly
// different.
func TestPauseAndResumeSchedule(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))

	rec := apiCall(t, srv, http.MethodPut, "/api/v1/schedules/"+created.ID, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("pause = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	paused := decodeSchedule(t, rec)
	if paused.Enabled {
		t.Error("the schedule is still enabled after being paused")
	}
	if paused.Time != "02:30" || paused.Frequency != "daily" {
		t.Errorf("pausing changed the timing: %+v", paused)
	}
	if _, ok := srv.sched.NextRun(created.ID); ok {
		t.Error("a paused schedule must not report a next run")
	}

	rec = apiCall(t, srv, http.MethodPut, "/api/v1/schedules/"+created.ID, map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := srv.sched.NextRun(created.ID); !ok {
		t.Error("a resumed schedule must come back onto the timetable")
	}
}

// Deleting a schedule stops the timetable and nothing else: the task and its backups stay.
func TestDeleteSchedule(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))

	if rec := apiCall(t, srv, http.MethodDelete, "/api/v1/schedules/"+created.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := srv.sched.NextRun(created.ID); ok {
		t.Error("a deleted schedule is still tracked")
	}
	if inst, err := srv.store.Instance("mysql-web01"); err != nil || inst == nil {
		t.Errorf("the instance went with its schedule: %v, %v", inst, err)
	}
	if rec := apiCall(t, srv, http.MethodDelete, "/api/v1/schedules/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", rec.Code)
	}
}

// The Schedules tab is one request: the state, the last run and the next run all arrive
// with the list, so the table does not need a request per row.
func TestListSchedulesCarriesStateAndLastRun(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))

	rec := apiCall(t, srv, http.MethodGet, "/api/v1/schedules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var list []scheduleView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("list = %d entries, want 1", len(list))
	}
	if list[0].State != scheduleNever {
		t.Errorf("state = %q, want %q before the first run", list[0].State, scheduleNever)
	}
	if list[0].Script != "mysql-backup" || list[0].NextRun == nil {
		t.Errorf("the list is missing the instance's script or the next run: %+v", list[0])
	}

	// A finished run of this schedule becomes its reported outcome.
	inst, _ := srv.store.Instance("mysql-web01")
	run, err := srv.store.CreateRun(inst, created.ID, 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	rec = apiCall(t, srv, http.MethodGet, "/api/v1/schedules", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list[0].State != scheduleFailed {
		t.Errorf("state = %q, want %q after a failed run", list[0].State, scheduleFailed)
	}
	if list[0].LastRun == nil || list[0].LastRun.ID != run.ID {
		t.Errorf("last run = %+v, want %s", list[0].LastRun, run.ID)
	}
}

// A manual run belongs to no schedule, so it must not be reported as one's outcome:
// a nightly backup that never ran would otherwise look like it had gone fine.
func TestManualRunIsNotASchedulesLastRun(t *testing.T) {
	srv := scheduleAPIServer(t)
	apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule())

	inst, _ := srv.store.Instance("mysql-web01")
	run, err := srv.store.CreateRun(inst, "", 60) // empty schedule id = "run now"
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	views, err := srv.scheduleViews("")
	if err != nil {
		t.Fatalf("scheduleViews: %v", err)
	}
	if views[0].LastRun != nil {
		t.Errorf("last run = %+v, want none — that run belonged to no schedule", views[0].LastRun)
	}
	if views[0].State != scheduleNever {
		t.Errorf("state = %q, want %q", views[0].State, scheduleNever)
	}
}

// The history of one task, which is how a run is reached now that there is no flat list.
func TestInstanceRunsPaging(t *testing.T) {
	srv := scheduleAPIServer(t)
	inst, _ := srv.store.Instance("mysql-web01")
	for i := 0; i < 5; i++ {
		if _, err := srv.store.CreateRun(inst, "", 60); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	rec := apiCall(t, srv, http.MethodGet, "/api/v1/instances/mysql-web01/runs?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Runs    []*Run `json:"runs"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(page.Runs) != 2 || !page.HasMore {
		t.Fatalf("page = %d runs, has_more=%v; want 2 and true", len(page.Runs), page.HasMore)
	}
	// Newest first: the run somebody is looking for is almost always the last one.
	if page.Runs[0].ID != "run-5" || page.Runs[1].ID != "run-4" {
		t.Errorf("page = %s, %s; want run-5 then run-4", page.Runs[0].ID, page.Runs[1].ID)
	}

	rec = apiCall(t, srv, http.MethodGet, "/api/v1/instances/mysql-web01/runs?limit=50", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Runs) != 5 || page.HasMore {
		t.Errorf("full page = %d runs, has_more=%v; want 5 and false", len(page.Runs), page.HasMore)
	}

	// Another task's runs must not appear in this one's history.
	rec = apiCall(t, srv, http.MethodGet, "/api/v1/instances/nobody/runs", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Runs) != 0 {
		t.Errorf("history of an unknown task = %d runs, want none", len(page.Runs))
	}
}

// The runs a schedule causes have to record which one it was, or a task's history cannot
// say what a run came from — the whole point of allowing more than one schedule.
func TestRunRecordsItsSchedule(t *testing.T) {
	srv := scheduleAPIServer(t)
	created := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))
	inst, _ := srv.store.Instance("mysql-web01")

	scheduled, err := srv.store.CreateRun(inst, created.ID, 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	manual, err := srv.store.CreateRun(inst, "", 60)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := srv.store.Run(scheduled.ID)
	if err != nil || got.ScheduleID != created.ID {
		t.Errorf("schedule_id = %q, %v; want %q", got.ScheduleID, err, created.ID)
	}
	got, err = srv.store.Run(manual.ID)
	if err != nil || got.ScheduleID != "" {
		t.Errorf("schedule_id = %q, %v; want empty for a manual run", got.ScheduleID, err)
	}
}

// An empty weekday column must come back as no weekdays at all. Splitting "" on a comma
// yields [""], which then fails to parse and takes the schedule with it.
func TestScheduleWeekdayRoundTrip(t *testing.T) {
	st, _ := openTestStore(t)
	daily, err := st.CreateSchedule(&Schedule{
		InstanceID:   "i",
		ScheduleJSON: ScheduleJSON{Frequency: "daily", Time: "02:00"},
		Enabled:      true,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	got, err := st.ScheduleByID(daily.ID)
	if err != nil {
		t.Fatalf("ScheduleByID: %v", err)
	}
	if len(got.Weekdays) != 0 {
		t.Errorf("weekdays = %v, want none", got.Weekdays)
	}
	if _, err := got.Spec(time.UTC); err != nil {
		t.Errorf("a schedule with no weekdays must still parse: %v", err)
	}

	weekly, err := st.CreateSchedule(&Schedule{
		InstanceID:   "i",
		ScheduleJSON: ScheduleJSON{Frequency: "weekly", Time: "02:00", Weekdays: []string{"mon", "thu"}},
		Enabled:      true,
	}, time.Now())
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	got, err = st.ScheduleByID(weekly.ID)
	if err != nil || len(got.Weekdays) != 2 || got.Weekdays[1] != "thu" {
		t.Errorf("weekdays = %v, %v; want [mon thu]", got.Weekdays, err)
	}
}

// Two schedules of one task coming due in the same minute describe the same work. Two
// dispatches would put two processes into one repository, so the check-in coalesces them
// into a single run — and still advances both, or the second would fire moments later.
func TestCheckinCoalescesSchedulesDueTogether(t *testing.T) {
	srv := scheduleAPIServer(t)
	nightly := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", validSchedule()))
	second := validSchedule()
	second["name"] = "second"
	second["time"] = "23:00"
	other := decodeSchedule(t, apiCall(t, srv, http.MethodPost, "/api/v1/schedules", second))

	// Both overdue: the nightly one by more, so it is the one the run is attributed to.
	now := time.Now()
	srv.sched.mu.Lock()
	srv.sched.st[nightly.ID].next = now.Add(-2 * time.Minute)
	srv.sched.st[other.ID].next = now.Add(-time.Minute)
	srv.sched.mu.Unlock()

	rec := checkinAs(srv, "web-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var res struct {
		Due []struct {
			RunID string `json:"run_id"`
		} `json:"due"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode checkin: %v (%s)", err, rec.Body.String())
	}
	if len(res.Due) != 1 {
		t.Fatalf("checkin dispatched %d jobs, want exactly 1", len(res.Due))
	}
	run, err := srv.store.Run(res.Due[0].RunID)
	if err != nil || run == nil {
		t.Fatalf("Run: %v", err)
	}
	if run.ScheduleID != nightly.ID {
		t.Errorf("schedule_id = %q, want %q — the one that came due first", run.ScheduleID, nightly.ID)
	}
	// Both advanced, so neither fires again on the next check-in a few seconds later.
	if due, _ := srv.sched.DueFor("mysql-web01", time.Now()); len(due) != 0 {
		t.Errorf("still due after the dispatch: %v", due)
	}
	if rec := checkinAs(srv, "web-01"); rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err == nil && len(res.Due) != 0 {
			t.Errorf("a second check-in dispatched %d more jobs, want none", len(res.Due))
		}
	}
}

// A "run now" on a task with no schedule at all has to reach the runner: an instance
// nobody has given a timetable is exactly the one somebody is testing by hand.
func TestCheckinDispatchesManualRunWithoutSchedule(t *testing.T) {
	srv := scheduleAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances/mysql-web01/run", nil); rec.Code != http.StatusOK {
		t.Fatalf("run now = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	rec := checkinAs(srv, "web-01")
	var res struct {
		Due []struct {
			RunID string `json:"run_id"`
		} `json:"due"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode checkin: %v (%s)", err, rec.Body.String())
	}
	if len(res.Due) != 1 {
		t.Fatalf("checkin dispatched %d jobs, want 1", len(res.Due))
	}
	run, err := srv.store.Run(res.Due[0].RunID)
	if err != nil || run.ScheduleID != "" {
		t.Errorf("schedule_id = %q, %v; want empty for a manual run", run.ScheduleID, err)
	}
	// And it is not dispatched twice.
	rec = checkinAs(srv, "web-01")
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err == nil && len(res.Due) != 0 {
		t.Errorf("the manual run was dispatched again (%d jobs)", len(res.Due))
	}
}
