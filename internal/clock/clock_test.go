package clock

import (
	"testing"
	"time"

	"brainbreak-lab/focus/internal/model"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return l
}

func TestAgeAt(t *testing.T) {
	dob := time.Date(2000, 3, 15, 0, 0, 0, 0, time.UTC)
	nyc := mustLoc(t, "America/New_York")
	sh := mustLoc(t, "Asia/Shanghai")

	cases := []struct {
		name string
		at   time.Time
		loc  *time.Location
		want int
	}{
		{"exact birthday UTC", time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC), time.UTC, 26},
		{"before birthday in UTC but after in SH", time.Date(2026, 3, 15, 19, 0, 0, 0, time.UTC), sh, 26},
		{"day before", time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC), time.UTC, 25},
		{"nyc before local midnight", time.Date(2026, 3, 15, 3, 0, 0, 0, time.UTC), nyc, 25}, // 2026-03-14 23:00 EDT
		{"nyc after local midnight", time.Date(2026, 3, 15, 4, 0, 0, 0, time.UTC), nyc, 26}, // 2026-03-15 00:00 EDT
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AgeAt(dob, c.at, c.loc); got != c.want {
				t.Fatalf("AgeAt=%d want %d", got, c.want)
			}
		})
	}
}

func TestAgeGroupBoundaries(t *testing.T) {
	if g := AgeGroupFor(0); g != model.AgeChild {
		t.Fatalf("0 -> %s", g)
	}
	if g := AgeGroupFor(12); g != model.AgeChild {
		t.Fatalf("12 -> %s", g)
	}
	if g := AgeGroupFor(13); g != model.AgeTeen {
		t.Fatalf("13 -> %s", g)
	}
	if g := AgeGroupFor(17); g != model.AgeTeen {
		t.Fatalf("17 -> %s", g)
	}
	if g := AgeGroupFor(18); g != model.AgeAdult {
		t.Fatalf("18 -> %s", g)
	}
}

func TestLocalDateTimezoneCrossDay(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	// 2026-07-24 22:00 UTC = 2026-07-25 06:00 Shanghai.
	utc := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC)
	d := LocalDate(utc, sh)
	y, m, day := d.Date()
	if y != 2026 || m != 7 || day != 25 {
		t.Fatalf("got %04d-%02d-%02d want 2026-07-25", y, m, day)
	}
	d2 := LocalDate(utc, time.UTC)
	y2, m2, d2d := d2.Date()
	if y2 != 2026 || m2 != 7 || d2d != 24 {
		t.Fatalf("utc got %04d-%02d-%02d", y2, m2, d2d)
	}
}

func TestBedtimeWindowAndOverlap(t *testing.T) {
	nyc := mustLoc(t, "America/New_York")
	bedtime := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC) // 22:00
	at := time.Date(2026, 7, 24, 23, 0, 0, 0, nyc)       // 23:00 local (EDT)
	start, end := BedtimeWindow(bedtime, at, nyc)
	wantStart := time.Date(2026, 7, 24, 21, 0, 0, 0, nyc)
	wantEnd := time.Date(2026, 7, 24, 22, 0, 0, 0, nyc)
	if !start.Equal(wantStart) {
		t.Fatalf("start=%v want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Fatalf("end=%v want %v", end, wantEnd)
	}
	if !Overlaps(time.Date(2026,7,24,21,30,0,0,nyc), time.Date(2026,7,24,21,45,0,0,nyc), start, end) {
		t.Fatal("expected overlap")
	}
	if Overlaps(time.Date(2026,7,24,20,0,0,0,nyc), time.Date(2026,7,24,21,0,0,0,nyc), start, end) {
		t.Fatal("expected no overlap")
	}
}
