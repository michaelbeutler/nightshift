package schedule

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

var zurich = mustLoad("Europe/Zurich")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("CET", 3600)
	}
	return loc
}

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, zurich)
}

func TestParseClock(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"02:30":    2*time.Hour + 30*time.Minute,
		"2:30":     2*time.Hour + 30*time.Minute,
		"23:59:58": 23*time.Hour + 59*time.Minute + 58*time.Second,
		"2am":      2 * time.Hour,
		"12am":     0,
		"12pm":     12 * time.Hour,
		"11:45pm":  23*time.Hour + 45*time.Minute,
		"7 AM":     7 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseClock(in)
		if err != nil || got != want {
			t.Errorf("ParseClock(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "2", "24:00", "02:60", "13pm", "0am", "2:3", "a:b", "1:2:3:4"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) succeeded, want an error", bad)
		}
	}
}

func TestWindowNext(t *testing.T) {
	t.Parallel()
	night := Window{Start: time.Hour, End: 4 * time.Hour}
	late := Window{Start: 22 * time.Hour, End: 3 * time.Hour}
	cases := []struct {
		name     string
		w        Window
		now      time.Time
		from, to time.Time
	}{
		{"afternoon, window tonight", night, at(24, 17, 0), at(25, 1, 0), at(25, 4, 0)},
		{"inside the window", night, at(25, 2, 0), at(25, 2, 0), at(25, 4, 0)},
		{"after the window", night, at(25, 5, 0), at(26, 1, 0), at(26, 4, 0)},
		{"spanning midnight, before", late, at(24, 17, 0), at(24, 22, 0), at(25, 3, 0)},
		{"spanning midnight, after midnight", late, at(25, 1, 0), at(25, 1, 0), at(25, 3, 0)},
		{"spanning midnight, before midnight", late, at(24, 23, 0), at(24, 23, 0), at(25, 3, 0)},
	}
	for _, c := range cases {
		from, to := c.w.Next(c.now)
		if !from.Equal(c.from) || !to.Equal(c.to) {
			t.Errorf("%s: Next(%v) = [%v, %v), want [%v, %v)", c.name, c.now, from, to, c.from, c.to)
		}
	}
}

func TestWindowPickStaysInside(t *testing.T) {
	t.Parallel()
	w := Window{Start: time.Hour, End: 4 * time.Hour}
	r := rand.New(rand.NewPCG(1, 2))
	now := at(24, 17, 0)
	for range 1000 {
		p := w.Pick(now, r)
		if p.Before(at(25, 1, 0)) || !p.Before(at(25, 4, 0)) || p.Nanosecond() != 0 {
			t.Fatalf("Pick = %v, outside the window or not whole seconds", p)
		}
	}
}

func TestParseSpacing(t *testing.T) {
	t.Parallel()
	s, err := ParseSpacing("3m-15m")
	if err != nil || s.Min != 3*time.Minute || s.Max != 15*time.Minute {
		t.Fatalf("ParseSpacing = %+v, %v", s, err)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 1000 {
		if d := s.Pick(r); d < s.Min || d > s.Max {
			t.Fatalf("Pick = %v, outside %v", d, s)
		}
	}
	if s, _ := ParseSpacing("0s"); s.Pick(r) != time.Second {
		t.Errorf("a zero spacing must still separate items by a second")
	}
	for _, bad := range []string{"", "x", "5m-1m", "-1m"} {
		if _, err := ParseSpacing(bad); err == nil {
			t.Errorf("ParseSpacing(%q) succeeded, want an error", bad)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	w := Window{Start: time.Hour, End: 4 * time.Hour}
	now := at(24, 17, 30) // a Tuesday
	cases := map[string]time.Time{
		"now":              now,
		"+2h":              now.Add(2 * time.Hour),
		"in 90m":           now.Add(90 * time.Minute),
		"02:30":            at(25, 2, 30),
		"18:00":            at(24, 18, 0),
		"2am":              at(25, 2, 0),
		"today 23:00":      at(24, 23, 0),
		"tonight 02:00":    at(25, 2, 0),
		"tomorrow 1am":     at(25, 1, 0),
		"tomorrow 2 am":    at(25, 2, 0),
		"fri 02:00":        at(27, 2, 0),
		"tue 18:00":        at(24, 18, 0),
		"tue 09:00":        time.Date(2026, 12, 1, 9, 0, 0, 0, zurich),
		"2026-12-01 02:00": time.Date(2026, 12, 1, 2, 0, 0, 0, zurich),
		"2026-12-01T02:00": time.Date(2026, 12, 1, 2, 0, 0, 0, zurich),
	}
	r := rand.New(rand.NewPCG(5, 6))
	for in, want := range cases {
		got, err := Parse(in, now, w, r)
		if err != nil || !got.Equal(want) {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "tonight", "auto", "01:30-02:00"} {
		got, err := Parse(in, now, w, r)
		if err != nil || got.Before(at(25, 1, 0)) || !got.Before(at(25, 4, 0)) {
			t.Errorf("Parse(%q) = %v, %v; want a time in tonight's window", in, got, err)
		}
	}
	for _, in := range []string{"today 09:00", "2026-11-01 02:00"} {
		if _, err := Parse(in, now, w, r); !errors.Is(err, ErrPast) {
			t.Errorf("Parse(%q) error = %v, want ErrPast", in, err)
		}
	}
	for _, in := range []string{"someday", "+-2h", "blursday 02:00", "in soon"} {
		if _, err := Parse(in, now, w, r); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", in)
		}
	}
}

func TestParseAcrossDST(t *testing.T) {
	t.Parallel()
	// Europe/Zurich leaves summer time on 2026-10-25 at 03:00.
	if zurich.String() != "Europe/Zurich" {
		t.Skip("no time zone database")
	}
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, zurich)
	got, err := Parse("tomorrow 04:00", now, Window{Start: time.Hour, End: 4 * time.Hour}, rand.New(rand.NewPCG(1, 1)))
	if err != nil || got.Hour() != 4 || got.Day() != 25 {
		t.Fatalf("Parse across DST = %v, %v; want 04:00 wall clock on the 25th", got, err)
	}
}

func TestResolveExactAndLoose(t *testing.T) {
	t.Parallel()
	w := Window{Start: time.Hour, End: 4 * time.Hour}
	now := at(24, 17, 30) // Tuesday 2026-11-24 17:30, Europe/Zurich
	dec1 := func(h, m, s int) time.Time { return time.Date(2026, 12, 1, h, m, s, 0, zurich) }
	exactCases := map[string]time.Time{
		"2026-12-01 02:00":            dec1(2, 0, 0),
		"2026-12-01 02:00:30":         dec1(2, 0, 30),
		"2026-12-01T02:00":            dec1(2, 0, 0),
		"2026/12/01 02:00":            dec1(2, 0, 0),
		"01.12.2026 02:00":            dec1(2, 0, 0),
		"1.12.2026 2:00":              dec1(2, 0, 0),
		"dec 1 02:00":                 dec1(2, 0, 0),
		"Dec 1 at 2 am":               dec1(2, 0, 0),
		"1 dec 2026 2am":              dec1(2, 0, 0),
		"December 1st, 2026 at 02:00": dec1(2, 0, 0),
		"on 1.12. at 02:00":           dec1(2, 0, 0),
		"jan 5 02:00":                 time.Date(2027, 1, 5, 2, 0, 0, 0, zurich),
		"nov 24 09:00":                time.Date(2027, 11, 24, 9, 0, 0, 0, zurich),
		"tomorrow 01:30":              at(25, 1, 30),
		"fri at 02:00":                at(27, 2, 0),
		"02:30":                       at(25, 2, 30),
		"+2h":                         now.Add(2 * time.Hour),
		"2026-12-01 02:00 UTC":        dec1(3, 0, 0),
		"2026-12-01 02:00 +05:30":     time.Date(2026, 11, 30, 21, 30, 0, 0, zurich),
		"2026-12-01T02:00:00Z":        dec1(3, 0, 0),
		"2026-12-01T02:00:00+01:00":   dec1(2, 0, 0),
		"@" + strconv.FormatInt(dec1(2, 0, 0).Unix(), 10): dec1(2, 0, 0),
	}
	if zurich.String() == "Europe/Zurich" {
		exactCases["02:00 America/New_York"] = at(25, 8, 0)
		exactCases["2026-12-01 02:00 Europe/London"] = dec1(3, 0, 0)
	}
	r := rand.New(rand.NewPCG(8, 9))
	for in, want := range exactCases {
		got, err := Resolve(in, now, w, r)
		if err != nil || !got.Exact || !got.Time.Equal(want) {
			t.Errorf("Resolve(%q) = %v exact=%v, %v; want exactly %v", in, got.Time, got.Exact, err, want)
		}
	}

	looseCases := map[string][2]time.Time{
		"":            {at(25, 1, 0), at(25, 4, 0)},
		"tonight":     {at(25, 1, 0), at(25, 4, 0)},
		"now":         {now, now.Add(time.Second)},
		"01:30-02:00": {at(25, 1, 30), at(25, 2, 0)},
		"2026-12-01":  {dec1(1, 0, 0), dec1(4, 0, 0)},
		"dec 1":       {dec1(1, 0, 0), dec1(4, 0, 0)},
		"fri":         {at(27, 1, 0), at(27, 4, 0)},
		"tue":         {dec1(1, 0, 0), dec1(4, 0, 0)}, // today's window is over
	}
	for in, span := range looseCases {
		got, err := Resolve(in, now, w, r)
		if err != nil || got.Exact || got.Time.Before(span[0]) || !got.Time.Before(span[1]) {
			t.Errorf("Resolve(%q) = %v exact=%v, %v; want a loose time in [%v, %v)", in, got.Time, got.Exact, err, span[0], span[1])
		}
	}

	for _, in := range []string{"2026-11-01 02:00", "@1", "2026-11-20", "today 09:00"} {
		if _, err := Resolve(in, now, w, r); !errors.Is(err, ErrPast) {
			t.Errorf("Resolve(%q) error = %v, want ErrPast", in, err)
		}
	}
	for _, in := range []string{"13.13.2026 02:00", "dec 1 25:00", "2026-12-01 02:00 Mars/Olympus", "someday soon"} {
		if _, err := Resolve(in, now, w, r); err == nil {
			t.Errorf("Resolve(%q) succeeded, want an error", in)
		}
	}
}
