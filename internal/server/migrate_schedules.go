package server

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"time"
)

// migrateInlineSchedules lifts each instance's old inline schedule JSON into a row of
// its own in the schedules table. It runs on every open and does nothing on a database
// that has already been through it.
//
// The guard is instances.schedule_migrated, set per row. The obvious alternative — "does
// this instance already have a schedule?" — is wrong in the one case that matters: an
// operator who deletes a schedule would get it back on the next restart, silently, and
// the task would keep running at a time they had removed. A per-row marker cannot do
// that, because the answer it records is "this instance's legacy blob has been dealt
// with", not "this instance has a schedule".
func migrateInlineSchedules(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, schedule FROM instances WHERE schedule_migrated = 0`)
	if err != nil {
		return err
	}
	type pending struct {
		id  string
		raw string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.raw); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(todo) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	created := 0
	for _, p := range todo {
		var sc ScheduleJSON
		if err := json.Unmarshal([]byte(p.raw), &sc); err != nil {
			// A server that refuses to start over a malformed legacy blob is worse than one
			// that starts with a task somebody has to reschedule by hand: the second keeps
			// every other backup running.
			log.Printf("schedule migration: instance %q has an unreadable schedule (%v); "+
				"create one for it under Schedules", p.id, err)
		} else if sc.Frequency != "" && sc.Time != "" {
			_, err := tx.Exec(`
				INSERT INTO schedules (instance_id, name, frequency, time, weekdays, day, timezone,
				  enabled, created_at, updated_at)
				VALUES (?, 'default', ?, ?, ?, ?, ?, 1, ?, ?)`,
				p.id, sc.Frequency, sc.Time, strings.Join(sc.Weekdays, ","), sc.Day, sc.Timezone,
				now, now)
			if err != nil {
				return err
			}
			created++
		} else {
			// Nothing to run: the instance was only ever triggered by hand, and still can be.
			log.Printf("schedule migration: instance %q had no usable schedule; "+
				"it stays runnable on demand", p.id)
		}
		if _, err := tx.Exec(`UPDATE instances SET schedule_migrated = 1 WHERE id = ?`, p.id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if created > 0 {
		log.Printf("schedule migration: moved %d inline schedule(s) into the schedules table", created)
	}
	return nil
}
