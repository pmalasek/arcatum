package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The period view exists to be trusted at a glance, which means the interesting cases are
// the ones where a figure could quietly become a lie: a run somebody stopped counted as a
// failure, a discarded payload counted as data, a rotated dump counted as occupied disk,
// or a night's runs landing in the wrong day.

func decodeStats(t *testing.T, srv *Server, query string) *statsResponse {
	t.Helper()
	rec := apiCall(t, srv, http.MethodGet, "/api/v1/stats"+query, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats%s = %d, want 200 (%s)", query, rec.Code, rec.Body.String())
	}
	var out statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode stats: %v (%s)", err, rec.Body.String())
	}
	return &out
}

// runAt records a finished run of instanceID with a payload, an exit code and a start and
// end of its own, so a window can be tested without waiting days for one.
func runAt(t *testing.T, st *Store, instanceID string, startedAt, endedAt time.Time, exitCode int, dataBytes int64) *Run {
	t.Helper()
	run, err := st.CreateRun(&Instance{ID: instanceID, Script: "mysql-backup", RunnerID: "db-01"}, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if dataBytes > 0 {
		if err := st.SetRunDataBytes(run.ID, dataBytes); err != nil {
			t.Fatalf("SetRunDataBytes: %v", err)
		}
	}
	if err := st.FinishRun(run.ID, endedAt, exitCode, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	n, err := parseRunID(run.ID)
	if err != nil {
		t.Fatalf("parseRunID: %v", err)
	}
	// CreateRun stamps started_at with now; the window under test is in the past.
	if _, err := st.db.Exec(`UPDATE runs SET started_at = ?, ended_at = ? WHERE id = ?`,
		toMillis(startedAt), toMillis(endedAt), n); err != nil {
		t.Fatalf("backdate run: %v", err)
	}
	return run
}

// day is local midnight n days ago plus an hour offset, in the scheduler's timezone —
// the same clock the buckets are cut on.
func day(srv *Server, daysAgo int, hour int) time.Time {
	loc := srv.sched.Location()
	now := time.Now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return midnight.AddDate(0, 0, -daysAgo).Add(time.Duration(hour) * time.Hour)
}

// Seven days asked for is seven days returned, oldest first, with the empty ones present
// as zeros: a chart that skips quiet days draws a busy week that never happened.
func TestStatsBucketsCoverEveryDay(t *testing.T) {
	srv := instanceAPIServer(t)
	runAt(t, srv.store, "mysql-web01", day(srv, 2, 1), day(srv, 2, 2), 0, 1024)

	out := decodeStats(t, srv, "?days=7")
	if out.Days != 7 || len(out.Daily) != 7 {
		t.Fatalf("days = %d, buckets = %d, want 7 and 7", out.Days, len(out.Daily))
	}
	for i := 1; i < len(out.Daily); i++ {
		if out.Daily[i-1].Day >= out.Daily[i].Day {
			t.Fatalf("buckets not oldest first: %q then %q", out.Daily[i-1].Day, out.Daily[i].Day)
		}
	}
	want := day(srv, 2, 0).Format("2006-01-02")
	filled := 0
	for _, b := range out.Daily {
		if b.Success+b.Failed+b.Cancelled > 0 {
			filled++
			if b.Day != want {
				t.Fatalf("run landed on %q, want %q", b.Day, want)
			}
			if b.DataBytes != 1024 {
				t.Fatalf("bucket data_bytes = %d, want 1024", b.DataBytes)
			}
		}
	}
	if filled != 1 {
		t.Fatalf("%d buckets have runs, want 1", filled)
	}
}

// The window is a boundary, and a boundary is only real if something falls outside it.
func TestStatsWindowBoundary(t *testing.T) {
	srv := instanceAPIServer(t)
	runAt(t, srv.store, "mysql-web01", day(srv, 8, 1), day(srv, 8, 2), 0, 512)

	if week := decodeStats(t, srv, "?days=7"); week.Period.Completed != 0 {
		t.Fatalf("7-day window completed = %d, want 0", week.Period.Completed)
	}
	month := decodeStats(t, srv, "?days=30")
	if month.Period.Completed != 1 || month.Period.Success != 1 {
		t.Fatalf("30-day window = %d completed / %d success, want 1 and 1",
			month.Period.Completed, month.Period.Success)
	}
	if len(month.Daily) != 30 {
		t.Fatalf("30-day window has %d buckets", len(month.Daily))
	}
}

// A run somebody stopped is not a failed backup. It must not drag the success rate down,
// or the figure becomes one operators learn to ignore.
func TestStatsCancelledOutsideSuccessRate(t *testing.T) {
	srv := instanceAPIServer(t)
	inst := &Instance{ID: "mysql-web01", Script: "mysql-backup", RunnerID: "db-01"}
	run, err := srv.store.CreateRun(inst, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := srv.store.RequestRunCancel(run.ID); err != nil {
		t.Fatalf("RequestRunCancel: %v", err)
	}
	if err := srv.store.FinishRun(run.ID, day(srv, 1, 3), 1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runAt(t, srv.store, "mysql-web01", day(srv, 1, 1), day(srv, 1, 2), 0, 100)

	out := decodeStats(t, srv, "?days=7")
	if out.Period.Cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", out.Period.Cancelled)
	}
	if out.Period.Completed != 1 || out.Period.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, want 1 and 0", out.Period.Completed, out.Period.Failed)
	}
	if out.Period.SuccessRate != 1 {
		t.Fatalf("success_rate = %v, want 1", out.Period.SuccessRate)
	}
}

// A run the runner never picked up has no duration to report. Counting it as a zero-length
// backup would flatter every average that includes it.
func TestStatsNeverStartedHasNoDuration(t *testing.T) {
	srv := instanceAPIServer(t)
	inst := &Instance{ID: "mysql-web01", Script: "mysql-backup", RunnerID: "db-01"}
	run, err := srv.store.CreateRun(inst, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// What the reaper leaves behind: an error with no start (reaper.go).
	if err := srv.store.FinishRun(run.ID, day(srv, 1, 4), -1, "runner never started this run"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	n, _ := parseRunID(run.ID)
	if _, err := srv.store.db.Exec(`UPDATE runs SET started_at = 0, ended_at = ? WHERE id = ?`,
		toMillis(day(srv, 1, 4)), n); err != nil {
		t.Fatalf("clear started_at: %v", err)
	}

	out := decodeStats(t, srv, "?days=7")
	if out.Period.NeverStarted != 1 || out.Period.Failed != 1 {
		t.Fatalf("never_started = %d, failed = %d, want 1 and 1",
			out.Period.NeverStarted, out.Period.Failed)
	}
	if out.Period.DurationRuns != 0 || out.Period.DurationMs != 0 || out.Period.AvgDurationMs != 0 {
		t.Fatalf("duration counted for a run that never started: %+v", out.Period)
	}
}

// The payload of a failed run is discarded (store.go), so reporting its size would be
// reporting data that does not exist.
func TestStatsFailedRunContributesNoData(t *testing.T) {
	srv := instanceAPIServer(t)
	runAt(t, srv.store, "mysql-web01", day(srv, 1, 1), day(srv, 1, 2), 1, 4096)
	runAt(t, srv.store, "mysql-web01", day(srv, 1, 3), day(srv, 1, 4), 0, 2048)

	out := decodeStats(t, srv, "?days=7")
	if out.Period.DataBytes != 2048 {
		t.Fatalf("data_bytes = %d, want 2048 (the failed run's payload was discarded)", out.Period.DataBytes)
	}
	if out.Period.Failed != 1 || out.Period.Success != 1 {
		t.Fatalf("failed = %d, success = %d, want 1 and 1", out.Period.Failed, out.Period.Success)
	}
	if out.Period.SuccessRate != 0.5 {
		t.Fatalf("success_rate = %v, want 0.5", out.Period.SuccessRate)
	}
}

// Throughput and occupancy are different questions. A dump rotated away last night was
// still backed up this week, and is still gone from the disk.
func TestStatsPrunedDumpCountsAsThroughputNotStorage(t *testing.T) {
	srv := instanceAPIServer(t)
	run := runAt(t, srv.store, "mysql-web01", day(srv, 1, 1), day(srv, 1, 2), 0, 8192)
	if _, err := srv.store.DeleteRunPayload(run.ID); err != nil {
		t.Fatalf("DeleteRunPayload: %v", err)
	}

	out := decodeStats(t, srv, "?days=7")
	if out.Period.DataBytes != 8192 {
		t.Fatalf("data_bytes = %d, want 8192 — rotation does not unmake a backup", out.Period.DataBytes)
	}
	if out.Storage.DumpBytes != 0 {
		t.Fatalf("storage.dump_bytes = %d, want 0 — the payload is gone", out.Storage.DumpBytes)
	}
}

// A backup that begins at 23:50 and finishes after midnight belongs to the night it
// started, which is the night an operator will look at when it goes wrong.
func TestStatsRunAcrossMidnightBelongsToItsStartDay(t *testing.T) {
	srv := instanceAPIServer(t)
	runAt(t, srv.store, "mysql-web01", day(srv, 2, 23).Add(50*time.Minute), day(srv, 1, 0).Add(10*time.Minute), 0, 64)

	out := decodeStats(t, srv, "?days=7")
	want := day(srv, 2, 0).Format("2006-01-02")
	for _, b := range out.Daily {
		if b.Success > 0 && b.Day != want {
			t.Fatalf("run bucketed on %q, want %q", b.Day, want)
		}
	}
}

// Per-instance figures are what the table sorts on, and the last result is what makes a
// row worth clicking.
func TestStatsPerInstance(t *testing.T) {
	srv := instanceAPIServer(t)
	runAt(t, srv.store, "small", day(srv, 1, 1), day(srv, 1, 2), 0, 100)
	runAt(t, srv.store, "big", day(srv, 1, 1), day(srv, 1, 3), 0, 5000)
	last := runAt(t, srv.store, "big", day(srv, 1, 4), day(srv, 1, 5), 1, 900)

	out := decodeStats(t, srv, "?days=7")
	if len(out.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(out.Instances))
	}
	if out.Instances[0].InstanceID != "big" {
		t.Fatalf("sorted by volume puts %q first, want \"big\"", out.Instances[0].InstanceID)
	}
	big := out.Instances[0]
	if big.Runs != 2 || big.Success != 1 || big.Failed != 1 {
		t.Fatalf("big: %d runs / %d success / %d failed, want 2/1/1", big.Runs, big.Success, big.Failed)
	}
	if big.DataBytes != 5000 {
		t.Fatalf("big data_bytes = %d, want 5000 (the failed run's payload is gone)", big.DataBytes)
	}
	if big.LastRunID != last.ID || big.LastStatus != StatusFailed {
		t.Fatalf("big last run = %s/%s, want %s/failed", big.LastRunID, big.LastStatus, last.ID)
	}
	if big.DurationMs != 3*time.Hour.Milliseconds() {
		t.Fatalf("big duration_ms = %d, want three hours", big.DurationMs)
	}
}

// A window is only meaningful if a nonsensical one is refused rather than obeyed.
func TestStatsDaysClamped(t *testing.T) {
	srv := instanceAPIServer(t)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", statsDefaultDays},
		{"?days=0", statsDefaultDays},
		{"?days=-1", statsDefaultDays},
		{"?days=abc", statsDefaultDays},
		{"?days=999", statsMaxDays},
		{"?days=30", 30},
	} {
		out := decodeStats(t, srv, tc.query)
		if out.Days != tc.want || len(out.Daily) != tc.want {
			t.Fatalf("stats%q: days = %d, buckets = %d, want %d",
				tc.query, out.Days, len(out.Daily), tc.want)
		}
	}
}

// The repository figure is a disk walk, and a disk walk must never be something a browser
// waits for. Before the first measurement lands the answer is "not measured yet", not a
// zero that reads as "nothing stored".
func TestStatsStorageNeverBlocks(t *testing.T) {
	srv := instanceAPIServer(t)
	done := make(chan *statsResponse, 1)
	go func() { done <- decodeStats(t, srv, "?days=7") }()
	select {
	case out := <-done:
		if !out.Storage.Stale || !out.Storage.MeasuredAt.IsZero() {
			t.Fatalf("first call reports a measurement it cannot have: %+v", out.Storage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stats did not answer within 5s — the disk walk is on the request path")
	}
}
