package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The period view: how the last N days went. It is deliberately a second endpoint rather
// than more fields on /dashboard, because the two answer questions on different clocks.
// The dashboard says what is happening *now* and is polled every few seconds; this is an
// aggregate over weeks, where a bucket only changes when a run finishes. Putting a
// thirty-day GROUP BY on the five-second path would make every open browser pay for it.

const (
	// statsDefaultDays is the window a caller gets without asking. A week is what an
	// operator compares against: it contains every schedule at least once.
	statsDefaultDays = 7
	// statsMaxDays bounds the scan. Beyond a quarter the chart is unreadable anyway.
	statsMaxDays = 90
	// statsInstanceLimit caps the per-instance table. Defensive: an installation with
	// thousands of tasks should not turn one request into a megabyte of JSON.
	statsInstanceLimit = 100
)

// statsResponse is the period view of the run history: how the window went, day by day
// and task by task, plus what all of it currently costs on disk.
type statsResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Days, From and To pin down exactly what was counted, so a figure on screen can be
	// checked against something. From is local midnight Days-1 days ago; To is the end of
	// today. Timezone is the scheduler's location — the day boundaries are its midnights,
	// which is the same clock the schedules themselves fire on.
	Days     int       `json:"days"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`

	Period    periodTotals   `json:"period"`
	Daily     []dayBucket    `json:"daily"`     // exactly Days entries, oldest first, gaps zero-filled
	Instances []instanceStat `json:"instances"` // every task with a finished run in the window
	Storage   storageSummary `json:"storage"`
}

// periodTotals is the strip of figures above the chart.
type periodTotals struct {
	// Completed is what the success rate is computed over: runs that reached a verdict.
	// Cancelled runs are outside it on purpose — somebody stopped those, and counting a
	// deliberate act against the success rate teaches operators to ignore the figure
	// (the same reasoning as RecentFailures, dashboard.go).
	Completed int `json:"completed"` // success + failed + error
	Success   int `json:"success"`
	Failed    int `json:"failed"` // failed and error together: both mean no backup
	Cancelled int `json:"cancelled"`
	// NeverStarted are runs closed without the runner ever picking them up (reaper.go).
	// They are failures with no duration to report, and worth their own figure because
	// their cause is elsewhere: a runner that is gone, not a backup that broke.
	NeverStarted int `json:"never_started"`
	// SuccessRate is Success/Completed, 0 when nothing completed. Sent rather than left
	// to the client, so every consumer divides by the same denominator.
	SuccessRate float64 `json:"success_rate"`
	// DataBytes is what was backed up in the period: the payload of successful runs. A
	// failed run's dump is discarded (store.go), so counting it would report data that
	// does not exist. This is throughput, not occupancy — see Storage for what is held.
	DataBytes int64 `json:"data_bytes"`
	// DurationMs sums only runs with both timestamps; DurationRuns says how many those
	// were, so an average taken over a partial set is visibly partial.
	DurationMs    int64 `json:"duration_ms"`
	AvgDurationMs int64 `json:"avg_duration_ms"`
	DurationRuns  int   `json:"duration_runs"`
}

// dayBucket is one column of the chart. Day is a local calendar date rather than a
// timestamp: the question is "how did Tuesday go", and Tuesday is a thing in a timezone.
type dayBucket struct {
	Day        string `json:"day"` // "2006-01-02", local to statsResponse.Timezone
	Success    int    `json:"success"`
	Failed     int    `json:"failed"`
	Cancelled  int    `json:"cancelled"`
	DataBytes  int64  `json:"data_bytes"`
	DurationMs int64  `json:"duration_ms"`
}

// instanceStat is one task's contribution to the period.
type instanceStat struct {
	InstanceID    string `json:"instance_id"`
	Runs          int    `json:"runs"`
	Success       int    `json:"success"`
	Failed        int    `json:"failed"`
	DataBytes     int64  `json:"data_bytes"`
	DurationMs    int64  `json:"duration_ms"`
	AvgDurationMs int64  `json:"avg_duration_ms"`
	// The last run in the window, so a row says how the task is doing as well as what it
	// cost. Taken from the scan itself rather than a second query: the rows arrive in
	// ended_at order, so the last one seen per task is the latest one.
	LastRunID   string    `json:"last_run_id"`
	LastStatus  RunStatus `json:"last_status"`
	LastEndedAt time.Time `json:"last_ended_at"`
}

// storageSummary is what Arcatum is holding right now. The three figures mean different
// things and are deliberately not added into one number.
type storageSummary struct {
	// DumpBytes is the payload of successful runs that retention has not yet rotated
	// away: exactly the files the restore view offers for download. Accounted from the
	// database, so it is what the records claim rather than a measurement.
	DumpBytes int64 `json:"dump_bytes"`
	// LogBytes is captured output still on disk (logs_pruned = 0).
	LogBytes int64 `json:"log_bytes"`
	// RepoBytes is measured on disk, because a restic repository is deduplicated and the
	// database cannot know its size. Repositories is how many there are.
	RepoBytes    int64 `json:"repo_bytes"`
	Repositories int   `json:"repositories"`
	// MeasuredAt is when the walk last finished; zero means never yet, which the UI shows
	// as "measuring" rather than as zero bytes. Stale says a fresh walk is under way and
	// this is the previous figure — an ageing number, never a slow page.
	MeasuredAt time.Time `json:"measured_at"`
	Stale      bool      `json:"stale"`
}

// handleStats answers the period view for the last `days` days.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days := intParam(r, "days", statsDefaultDays, statsMaxDays)
	// intParam only guards negatives, and an explicit days=0 would ask for an empty
	// window — which is a typo, not a request.
	if days <= 0 {
		days = statsDefaultDays
	}

	loc := s.sched.Location()
	now := time.Now().In(loc)
	// The window ends at the end of today, so today's runs are in it, and begins at the
	// midnight `days-1` days back, so it holds exactly `days` whole calendar days.
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	from := to.AddDate(0, 0, -days)

	period, daily, instances, err := s.store.PeriodStats(from, to, loc, statsInstanceLimit)
	if err != nil {
		s.log.Printf("stats: period scan: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	storage, err := s.storageSummary()
	if err != nil {
		s.log.Printf("stats: storage: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, statsResponse{
		GeneratedAt: now,
		Days:        days,
		From:        from,
		To:          to,
		Timezone:    loc.String(),
		Period:      period,
		Daily:       daily,
		Instances:   instances,
		Storage:     storage,
	})
}

// statsRunCols is deliberately not runCols (store.go): that one ends in a correlated
// sub-select for the replication state, which costs a query per row and which an
// aggregate has no use for.
const statsRunCols = `id, instance_id, status, started_at, ended_at, data_bytes`

// PeriodStats walks the runs that finished inside [from, to) once and returns the totals,
// the per-day buckets and the per-instance breakdown together.
//
// One pass rather than three queries, for two reasons. The three answers are three views
// of the same rows, and computing them separately is how they come to disagree with each
// other on screen. And the day boundaries are computed here with a *time.Location, not
// with a SQL date modifier: 'localtime' would use the process timezone rather than the
// scheduler's, and a fixed '+HH:MM' offset gets the one day in the window that straddles
// a daylight-saving change wrong, moving a night's runs into the neighbouring bucket.
func (s *Store) PeriodStats(from, to time.Time, loc *time.Location, limit int) (periodTotals, []dayBucket, []instanceStat, error) {
	var period periodTotals

	// The buckets are seeded for every day in the window, so a day on which nothing ran
	// is a zero rather than a gap the chart has to guess at.
	days := int(to.Sub(from).Hours()/24 + 0.5)
	daily := make([]dayBucket, 0, days)
	index := make(map[string]int, days)
	for i := 0; i < days; i++ {
		key := from.AddDate(0, 0, i).Format("2006-01-02")
		index[key] = len(daily)
		daily = append(daily, dayBucket{Day: key})
	}

	rows, err := s.db.Query(`SELECT `+statsRunCols+`
		FROM runs WHERE ended_at >= ? AND ended_at < ? ORDER BY ended_at`,
		toMillis(from), toMillis(to))
	if err != nil {
		return period, nil, nil, err
	}
	defer rows.Close()

	byInstance := map[string]*instanceStat{}
	var order []string // first-seen order, so the sort below is deterministic
	for rows.Next() {
		var id, started, ended, dataBytes int64
		var instanceID, status string
		if err := rows.Scan(&id, &instanceID, &status, &started, &ended, &dataBytes); err != nil {
			return period, nil, nil, err
		}

		st := RunStatus(status)
		var ok, bad, cancelled bool
		switch st {
		case StatusSuccess:
			ok = true
		case StatusFailed, StatusError:
			bad = true
		case StatusCancelled:
			cancelled = true
		default:
			// A pending or running row with a non-zero ended_at should not exist. Skip it
			// rather than count it as something it is not.
			continue
		}

		// Duration needs both ends. A run the reaper closed without the runner ever
		// starting it has none, and contributes to no average.
		var durMs int64
		if started > 0 && ended > started {
			durMs = ended - started
		}
		// Only a successful run's payload still exists; a failed one's is discarded.
		var volume int64
		if ok {
			volume = dataBytes
		}

		period.Cancelled += boolInt(cancelled)
		period.Success += boolInt(ok)
		period.Failed += boolInt(bad)
		if ok || bad {
			period.Completed++
		}
		if started == 0 {
			period.NeverStarted++
		}
		period.DataBytes += volume
		if durMs > 0 {
			period.DurationMs += durMs
			period.DurationRuns++
		}

		// A run that began before the window but ended inside it has no bucket of its own
		// here. It stays in the totals and drops out of the chart, which is the honest
		// rendering: the chart is "what happened on each of these days".
		stamp := started
		if stamp == 0 {
			stamp = ended
		}
		if i, in := index[time.UnixMilli(stamp).In(loc).Format("2006-01-02")]; in {
			b := &daily[i]
			b.Success += boolInt(ok)
			b.Failed += boolInt(bad)
			b.Cancelled += boolInt(cancelled)
			b.DataBytes += volume
			b.DurationMs += durMs
		}

		agg := byInstance[instanceID]
		if agg == nil {
			agg = &instanceStat{InstanceID: instanceID}
			byInstance[instanceID] = agg
			order = append(order, instanceID)
		}
		agg.Runs++
		agg.Success += boolInt(ok)
		agg.Failed += boolInt(bad)
		agg.DataBytes += volume
		agg.DurationMs += durMs
		agg.LastRunID, agg.LastStatus, agg.LastEndedAt = formatRunID(id), st, fromMillis(ended)
	}
	if err := rows.Err(); err != nil {
		return period, nil, nil, err
	}

	if period.Completed > 0 {
		period.SuccessRate = float64(period.Success) / float64(period.Completed)
	}
	if period.DurationRuns > 0 {
		period.AvgDurationMs = period.DurationMs / int64(period.DurationRuns)
	}

	instances := make([]instanceStat, 0, len(order))
	for _, id := range order {
		agg := byInstance[id]
		if agg.Runs > 0 {
			agg.AvgDurationMs = agg.DurationMs / int64(agg.Runs)
		}
		instances = append(instances, *agg)
	}
	// Sorted by volume, because that is the question the table is usually opened with.
	// The client re-sorts by duration without a request, so both orders are one click
	// apart and neither costs a round trip.
	sort.Slice(instances, func(a, b int) bool {
		if instances[a].DataBytes != instances[b].DataBytes {
			return instances[a].DataBytes > instances[b].DataBytes
		}
		if instances[a].DurationMs != instances[b].DurationMs {
			return instances[a].DurationMs > instances[b].DurationMs
		}
		return instances[a].InstanceID < instances[b].InstanceID
	})
	if limit > 0 && len(instances) > limit {
		instances = instances[:limit]
	}
	return period, daily, instances, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// StoredBytes reports what is still on disk according to the records: the payload of
// successful runs retention has not rotated away, and the captured output not yet swept.
//
// Deliberately not ResetStats (reset.go): that one sums every run regardless of the
// pruned flags, which is right for "what a reset would remove from the database" and
// wrong as an occupancy figure — it counts dumps that were deleted weeks ago.
func (s *Store) StoredBytes() (dumpBytes, logBytes int64, err error) {
	err = s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN status = ? AND data_pruned = 0 THEN data_bytes ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN logs_pruned = 0 THEN bytes ELSE 0 END), 0)
		FROM runs`, string(StatusSuccess)).Scan(&dumpBytes, &logBytes)
	return dumpBytes, logBytes, err
}

// storageTTL is how long a measurement of the restic directory is served before a fresh
// walk is started. Repository size changes only when a backup or a prune runs, so minutes
// of staleness cost nothing and save an operator's disk a walk per open browser.
const storageTTL = 5 * time.Minute

// storageCache holds the last measurement of the restic directory. Measuring means
// walking every repository, which takes as long as the disk takes — far too long to do
// inside a request the dashboard polls. The figure is therefore refreshed in the
// background and served from here with its age attached: a slow disk makes the number
// older, never the page slower.
type storageCache struct {
	mu         sync.Mutex
	bytes      int64
	repos      int
	measuredAt time.Time
	measuring  bool
}

// storageSummary returns the current storage figures, never blocking on the disk. If the
// measurement is stale it starts a fresh walk in the background and answers with the
// previous one, marked stale.
func (s *Server) storageSummary() (storageSummary, error) {
	dumpBytes, logBytes, err := s.store.StoredBytes()
	if err != nil {
		return storageSummary{}, err
	}
	out := storageSummary{DumpBytes: dumpBytes, LogBytes: logBytes}

	c := s.storage
	if c == nil {
		return out, nil
	}
	c.mu.Lock()
	out.RepoBytes, out.Repositories, out.MeasuredAt = c.bytes, c.repos, c.measuredAt
	fresh := !c.measuredAt.IsZero() && time.Since(c.measuredAt) < storageTTL
	start := !fresh && !c.measuring
	if start {
		c.measuring = true
	}
	out.Stale = !fresh
	c.mu.Unlock()

	if start {
		go s.measureStorage()
	}
	return out, nil
}

// measureStorage walks the restic directory once and records the result. One walk for the
// whole directory rather than a resticRepoInfo call per instance (restic.go): the same
// information for a fraction of the syscalls, and no chance of it creeping onto a
// request path.
func (s *Server) measureStorage() {
	bytes, repos, err := s.store.resticUsage()
	c := s.storage
	c.mu.Lock()
	defer c.mu.Unlock()
	c.measuring = false
	if err != nil {
		// Leave the previous figure and its timestamp alone: an unreadable directory is a
		// reason to keep saying "this is what we last saw", not to claim zero.
		s.log.Printf("stats: measure storage: %v", err)
		return
	}
	c.bytes, c.repos, c.measuredAt = bytes, repos, time.Now()
}

// resticUsage sums the file sizes under backup_dir/restic and counts the repositories.
func (s *Store) resticUsage() (int64, int, error) {
	root := filepath.Join(s.backupDir, "restic")
	entries, err := os.ReadDir(root)
	if err != nil {
		// No repositories yet is not an error: an installation may only ever stream dumps.
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	repos := 0
	for _, e := range entries {
		if e.IsDir() {
			repos++
		}
	}
	var total int64
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file removed by a prune while the walk is in flight is normal. Skipping it
			// makes the figure a few bytes stale; failing would make it absent.
			return nil //nolint:nilerr // see above
		}
		if d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total, repos, err
}
