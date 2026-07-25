package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// Wed 2026-07-08 12:00 local as the reference point.
	from := time.Date(2026, 7, 8, 12, 0, 0, 0, prague)

	tests := []struct {
		name string
		spec Spec
		want time.Time
	}{
		{
			name: "daily later today",
			spec: Spec{Frequency: Daily, Hour: 14, Minute: 30, Location: prague},
			want: time.Date(2026, 7, 8, 14, 30, 0, 0, prague),
		},
		{
			name: "daily already passed -> tomorrow",
			spec: Spec{Frequency: Daily, Hour: 2, Minute: 30, Location: prague},
			want: time.Date(2026, 7, 9, 2, 30, 0, 0, prague),
		},
		{
			name: "weekly mon/thu -> next thursday",
			spec: Spec{Frequency: Weekly, Hour: 2, Minute: 30,
				Weekdays: []time.Weekday{time.Monday, time.Thursday}, Location: prague},
			want: time.Date(2026, 7, 9, 2, 30, 0, 0, prague), // Thu
		},
		{
			name: "monthly day 1 -> next month",
			spec: Spec{Frequency: Monthly, Hour: 3, Minute: 0, Day: 1, Location: prague},
			want: time.Date(2026, 8, 1, 3, 0, 0, 0, prague),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.Next(from)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Next = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParseHHMM(t *testing.T) {
	h, m, err := ParseHHMM("02:30")
	if err != nil || h != 2 || m != 30 {
		t.Fatalf("ParseHHMM(02:30) = %d,%d,%v", h, m, err)
	}
	if _, _, err := ParseHHMM("25:00"); err == nil {
		t.Errorf("ParseHHMM(25:00) should fail")
	}
}
