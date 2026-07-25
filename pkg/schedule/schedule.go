// Package schedule computes when an instance is next due to run. The schedule
// belongs to an instance (not to the script definition) so the same script can
// run against different targets at different times.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Frequency of a schedule.
type Frequency string

const (
	Daily   Frequency = "daily"
	Weekly  Frequency = "weekly"
	Monthly Frequency = "monthly"
)

// Spec describes an instance schedule.
type Spec struct {
	Frequency Frequency
	Hour      int
	Minute    int
	Weekdays  []time.Weekday // used by Weekly; empty means every day
	Day       int            // day of month (1-28) used by Monthly; 0 means 1
	Location  *time.Location
}

// Next returns the first run time strictly after `after`.
func (s Spec) Next(after time.Time) (time.Time, error) {
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	base := after.In(loc)
	// Scan day by day up to ~13 months to cover any monthly day.
	for i := 0; i <= 400; i++ {
		day := base.AddDate(0, 0, i)
		cand := time.Date(day.Year(), day.Month(), day.Day(), s.Hour, s.Minute, 0, 0, loc)
		if !cand.After(after) {
			continue
		}
		if s.matches(cand) {
			return cand, nil
		}
	}
	return time.Time{}, fmt.Errorf("schedule: no run time found within range")
}

func (s Spec) matches(t time.Time) bool {
	switch s.Frequency {
	case Daily:
		return true
	case Weekly:
		if len(s.Weekdays) == 0 {
			return true
		}
		for _, w := range s.Weekdays {
			if t.Weekday() == w {
				return true
			}
		}
		return false
	case Monthly:
		day := s.Day
		if day == 0 {
			day = 1
		}
		return t.Day() == day
	}
	return false
}

// ParseHHMM parses "HH:MM" into hour and minute.
func ParseHHMM(s string) (hour, min int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("time %q: expected HH:MM", s)
	}
	if hour, err = strconv.Atoi(parts[0]); err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("time %q: invalid hour", s)
	}
	if min, err = strconv.Atoi(parts[1]); err != nil || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("time %q: invalid minute", s)
	}
	return hour, min, nil
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWeekday parses a three-letter English weekday abbreviation.
func ParseWeekday(s string) (time.Weekday, error) {
	if w, ok := weekdayNames[strings.ToLower(strings.TrimSpace(s))]; ok {
		return w, nil
	}
	return 0, fmt.Errorf("weekday %q: expected one of sun,mon,tue,wed,thu,fri,sat", s)
}
