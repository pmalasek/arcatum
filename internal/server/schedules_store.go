package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrScheduleNotFound is returned when an update or delete names a schedule that is not
// there, mirroring ErrInstanceNotFound.
var ErrScheduleNotFound = errors.New("schedule not found")

const scheduleCols = `id, instance_id, name, frequency, time, weekdays, day, timezone, ` +
	`enabled, created_at, updated_at`

func formatScheduleID(n int64) string { return "sch-" + strconv.FormatInt(n, 10) }

func parseScheduleID(id string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(id, "sch-"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid schedule id %q", id)
	}
	return n, nil
}

// splitWeekdays turns the stored comma list back into a slice. An empty column must
// yield nil rather than [""], which would fail to parse as a weekday.
func splitWeekdays(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func scanSchedule(sc interface{ Scan(...any) error }) (*Schedule, error) {
	var s Schedule
	var id int64
	var weekdays string
	var enabled int
	var created, updated int64
	if err := sc.Scan(&id, &s.InstanceID, &s.Name, &s.Frequency, &s.Time, &weekdays, &s.Day,
		&s.Timezone, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	s.ID = formatScheduleID(id)
	s.Weekdays = splitWeekdays(weekdays)
	s.Enabled = enabled != 0
	s.CreatedAt, s.UpdatedAt = fromMillis(created), fromMillis(updated)
	return &s, nil
}

func (s *Store) querySchedules(query string, args ...any) ([]*Schedule, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Schedules returns every schedule, grouped by the instance it belongs to.
func (s *Store) Schedules() ([]*Schedule, error) {
	return s.querySchedules(`SELECT ` + scheduleCols + ` FROM schedules ORDER BY instance_id, id`)
}

// SchedulesForInstance returns one task's schedules, oldest first.
func (s *Store) SchedulesForInstance(instanceID string) ([]*Schedule, error) {
	return s.querySchedules(`SELECT `+scheduleCols+`
		FROM schedules WHERE instance_id = ? ORDER BY id`, instanceID)
}

// ScheduleByID returns one schedule, or nil if it does not exist.
func (s *Store) ScheduleByID(id string) (*Schedule, error) {
	n, err := parseScheduleID(id)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`SELECT `+scheduleCols+` FROM schedules WHERE id = ?`, n)
	sc, err := scanSchedule(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sc, err
}

// CreateSchedule stores a new schedule and returns it with the id it was given.
func (s *Store) CreateSchedule(sc *Schedule, now time.Time) (*Schedule, error) {
	ms := toMillis(now)
	res, err := s.db.Exec(`
		INSERT INTO schedules (instance_id, name, frequency, time, weekdays, day, timezone,
		  enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.InstanceID, sc.Name, sc.Frequency, sc.Time, strings.Join(sc.Weekdays, ","), sc.Day,
		sc.Timezone, boolToInt(sc.Enabled), ms, ms)
	if err != nil {
		return nil, err
	}
	n, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	out := *sc
	out.ID = formatScheduleID(n)
	out.CreatedAt, out.UpdatedAt = now, now
	return &out, nil
}

// UpdateSchedule overwrites a schedule's timing, label and enabled flag. The instance it
// belongs to is not among them: a schedule does not move between tasks, because the
// history already recorded against it would move with it and stop making sense.
func (s *Store) UpdateSchedule(sc *Schedule, now time.Time) error {
	n, err := parseScheduleID(sc.ID)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`
		UPDATE schedules SET name = ?, frequency = ?, time = ?, weekdays = ?, day = ?,
		  timezone = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		sc.Name, sc.Frequency, sc.Time, strings.Join(sc.Weekdays, ","), sc.Day, sc.Timezone,
		boolToInt(sc.Enabled), toMillis(now), n)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// DeleteSchedule removes one schedule.
func (s *Store) DeleteSchedule(id string) error {
	n, err := parseScheduleID(id)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, n)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// CountSchedules reports how many schedules exist and how many of those are paused.
func (s *Store) CountSchedules() (total, disabled int, err error) {
	err = s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN enabled = 0 THEN 1 ELSE 0 END), 0)
		FROM schedules`).Scan(&total, &disabled)
	return total, disabled, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
