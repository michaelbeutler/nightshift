// Package schedule turns the times people type ("tonight", "02:30",
// "tomorrow 1am", "+2h") into instants, and picks natural-looking slots
// inside the nightly window.
package schedule

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrPast is returned for an explicit time that has already gone by.
var ErrPast = errors.New("that time is in the past")

// Window is a daily range of local time, such as 01:00-04:00. When End is
// earlier than Start the window spans midnight (22:00-03:00).
type Window struct {
	Start, End time.Duration // offsets from local midnight
}

// ParseWindow parses "HH:MM-HH:MM" (each side also accepts "2am" style).
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	start, err := ParseClock(a)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w", s, err)
	}
	end, err := ParseClock(b)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w", s, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("window %q is empty", s)
	}
	return Window{Start: start, End: end}, nil
}

// String formats the window as "HH:MM-HH:MM".
func (w Window) String() string { return FormatClock(w.Start) + "-" + FormatClock(w.End) }

// Next returns the next occurrence of the window that still has time left
// after now, as [from, to). When now falls inside the window, from is now.
func (w Window) Next(now time.Time) (from, to time.Time) {
	today := midnight(now)
	for d := -1; d <= 1; d++ {
		day := today.AddDate(0, 0, d)
		start := atClock(day, w.Start)
		endDay := day
		if w.End <= w.Start {
			endDay = day.AddDate(0, 0, 1)
		}
		end := atClock(endDay, w.End)
		if now.Before(end) {
			if now.After(start) {
				return now, end
			}
			return start, end
		}
	}
	// Unreachable: tomorrow's occurrence always ends after now.
	start := atClock(today.AddDate(0, 0, 2), w.Start)
	return start, start.Add(time.Hour)
}

// Pick returns a random instant, to the second, in the next occurrence of
// the window.
func (w Window) Pick(now time.Time, r *rand.Rand) time.Time {
	from, to := w.Next(now)
	span := to.Sub(from)
	if span <= time.Second {
		return from.Truncate(time.Second)
	}
	return from.Add(time.Duration(r.Int64N(int64(span)))).Truncate(time.Second)
}

// Spacing is the random gap between two consecutive scheduled items, so a
// series of commits does not land in the same second.
type Spacing struct {
	Min, Max time.Duration
}

// ParseSpacing parses "3m-15m" or a single duration such as "5m".
func ParseSpacing(s string) (Spacing, error) {
	s = strings.TrimSpace(s)
	a, b, ranged := strings.Cut(s, "-")
	lo, err := time.ParseDuration(strings.TrimSpace(a))
	if err != nil {
		return Spacing{}, fmt.Errorf("spacing %q: %w", s, err)
	}
	hi := lo
	if ranged {
		if hi, err = time.ParseDuration(strings.TrimSpace(b)); err != nil {
			return Spacing{}, fmt.Errorf("spacing %q: %w", s, err)
		}
	}
	if lo < 0 || hi < lo {
		return Spacing{}, fmt.Errorf("spacing %q: want MIN-MAX with 0 <= MIN <= MAX", s)
	}
	return Spacing{Min: lo, Max: hi}, nil
}

// Pick returns a random gap in [Min, Max], to the second, and at least one
// second so that ordered items never share a timestamp.
func (s Spacing) Pick(r *rand.Rand) time.Duration {
	d := s.Min
	if s.Max > s.Min {
		d += time.Duration(r.Int64N(int64(s.Max - s.Min)))
	}
	d = d.Truncate(time.Second)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Instant is a resolved time.
type Instant struct {
	Time time.Time
	// Exact is true when the time names a moment: a date and time, a time of
	// day, a delay or a Unix timestamp. It is false for "tonight", "now", a
	// window or a day, which leave nightshift free to pick the moment.
	Exact bool
}

// Parse resolves spec relative to now; see Resolve.
func Parse(spec string, now time.Time, w Window, r *rand.Rand) (time.Time, error) {
	in, err := Resolve(spec, now, w, r)
	return in.Time, err
}

// Resolve resolves spec relative to now. It accepts:
//
//	(empty), tonight, auto      a random moment in the next occurrence of w
//	now                         right away
//	01:00-03:00                 a random moment in the next such window
//	2026-12-01, dec 1, fri      a random moment in that day's window
//	+90m, in 2h                 exactly that long from now
//	02:30, 2:30am, 11pm         the next time the clock shows that
//	today 23:00, tomorrow 1am   that day at that time
//	fri 02:00                   the next Friday at that time
//	2026-12-01 02:00[:05]       exactly then; also 2026-12-01T02:00,
//	                            2026/12/01 02:00, 01.12.2026 02:00,
//	                            1 dec 2026 2am, december 1st, 2026 at 02:00
//	dec 1 02:00, 1.12. 02:00    the next such date (this year or next)
//	2026-12-01T02:00:00+01:00   RFC 3339
//	@1796090400                 a Unix timestamp
//
// A time zone may follow any of these: "UTC", "Z", "+01:00", "-0500", an
// IANA name ("Europe/Zurich", "America/New_York") or an upper-case
// abbreviation the time zone database knows ("CET"). Times are otherwise
// local. An exact time that has passed is an error wrapping ErrPast.
func Resolve(spec string, now time.Time, w Window, r *rand.Rand) (Instant, error) {
	raw := strings.TrimSpace(spec)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return exact(spec, t.In(now.Location()), now)
	}
	local := now.Location()
	if body, loc := splitZone(raw); loc != nil {
		raw, now = body, now.In(loc)
	}
	in, err := resolve(spec, raw, now, w, r)
	if err != nil {
		return Instant{}, err
	}
	in.Time = in.Time.In(local)
	return in, nil
}

func resolve(spec, raw string, now time.Time, w Window, r *rand.Rand) (Instant, error) {
	s := strings.ToLower(raw)
	switch s {
	case "", "tonight", "auto":
		return Instant{Time: w.Pick(now, r)}, nil
	case "now":
		return Instant{Time: now.Truncate(time.Second)}, nil
	}
	if rest, ok := strings.CutPrefix(s, "@"); ok {
		sec, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Instant{}, fmt.Errorf("time %q: want @ and Unix seconds", spec)
		}
		return exact(spec, time.Unix(sec, 0).In(now.Location()), now)
	}
	if rest, ok := cutAny(s, "+", "in "); ok {
		d, err := time.ParseDuration(strings.ReplaceAll(rest, " ", ""))
		if err != nil || d <= 0 {
			return Instant{}, fmt.Errorf("time %q: want a positive duration such as +2h or in 90m", spec)
		}
		return Instant{Time: now.Add(d).Truncate(time.Second), Exact: true}, nil
	}
	if win, err := ParseWindow(s); err == nil {
		return Instant{Time: win.Pick(now, r)}, nil
	}
	for _, layout := range absoluteLayouts {
		if t, err := time.ParseInLocation(layout, raw, now.Location()); err == nil {
			return exact(spec, t, now)
		}
	}

	// [day] [at] [time of day], in any of the forms above.
	s = ordinalRe.ReplaceAllString(strings.ReplaceAll(s, ",", " "), "$1")
	var words []string
	for _, f := range strings.Fields(s) {
		if f != "at" && f != "on" {
			words = append(words, f)
		}
	}
	off, n, hasClock := trailingClock(words)
	day := strings.Join(words[:len(words)-n], " ")
	date, next, err := resolveDay(day, now, hasClock, off)
	if err != nil {
		return Instant{}, fmt.Errorf("time %q: %w", spec, err)
	}
	if !hasClock {
		in, err := windowOn(spec, date, now, w, r)
		if errors.Is(err, ErrPast) && next != nil {
			return windowOn(spec, next(date), now, w, r)
		}
		return in, err
	}
	t := atClock(date, off)
	if day == "" || day == "tonight" || day == "next" {
		if !t.After(now) {
			t = atClock(date.AddDate(0, 0, 1), off)
		}
		return Instant{Time: t, Exact: true}, nil
	}
	return exact(spec, t, now)
}

var absoluteLayouts = []string{
	"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04",
	"2006/01/02 15:04:05", "2006/01/02 15:04", "2.1.2006 15:04:05", "2.1.2006 15:04",
}

var ordinalRe = regexp.MustCompile(`\b(\d{1,2})(st|nd|rd|th)\b`)

// exact checks that an exact time is still ahead.
func exact(spec string, t, now time.Time) (Instant, error) {
	if !t.After(now) {
		return Instant{}, fmt.Errorf("time %q (%s): %w", spec, t.Format("2006-01-02 15:04:05 MST"), ErrPast)
	}
	return Instant{Time: t, Exact: true}, nil
}

// trailingClock reads a time of day from the end of words: "02:30", "2am",
// or "2 am" over two words.
func trailingClock(words []string) (off time.Duration, n int, ok bool) {
	if len(words) == 0 {
		return 0, 0, false
	}
	if off, err := ParseClock(words[len(words)-1]); err == nil {
		return off, 1, true
	}
	if len(words) >= 2 {
		if off, err := ParseClock(words[len(words)-2] + words[len(words)-1]); err == nil {
			return off, 2, true
		}
	}
	return 0, 0, false
}

// The dates resolveDay understands (month names match in any case). Those
// without a year mean the next such date.
var (
	dateLayouts = []string{
		"2006-01-02", "2006/01/02", "2.1.2006",
		"Jan 2 2006", "January 2 2006", "2 Jan 2006", "2 January 2006",
	}
	yearlessLayouts = []string{"Jan 2", "January 2", "2 Jan", "2 January", "2.1.", "2.1"}
)

// resolveDay turns the day part of a time into that day's local midnight
// (the empty day is today). For a day that recurs (a weekday, a date
// without a year), next returns its following occurrence.
func resolveDay(day string, now time.Time, hasClock bool, off time.Duration) (d time.Time, next func(time.Time) time.Time, err error) {
	today := midnight(now)
	switch day {
	case "", "tonight", "next", "today":
		return today, nil, nil
	case "tomorrow":
		return today.AddDate(0, 0, 1), nil, nil
	}
	if wd, ok := weekdays[day]; ok {
		d := today.AddDate(0, 0, (int(wd)-int(now.Weekday())+7)%7)
		if hasClock && !atClock(d, off).After(now) {
			d = d.AddDate(0, 0, 7)
		}
		return d, func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }, nil
	}
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, day, now.Location()); err == nil {
			return t, nil, nil
		}
	}
	for _, layout := range yearlessLayouts {
		t, err := time.ParseInLocation(layout, day, now.Location())
		if err != nil {
			continue
		}
		d := time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location())
		last := d.AddDate(0, 0, 1)
		if hasClock {
			last = atClock(d, off)
		}
		if !last.After(now) {
			d = d.AddDate(1, 0, 0)
		}
		return d, func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }, nil
	}
	return time.Time{}, nil, errors.New("not understood (try 02:30, tomorrow 1am, fri 02:00, +2h, 2026-12-01 02:00 or 01.12.2026 02:00)")
}

// windowOn picks a moment in the window that starts on day.
func windowOn(spec string, day, now time.Time, w Window, r *rand.Rand) (Instant, error) {
	start := atClock(day, w.Start)
	endDay := day
	if w.End <= w.Start {
		endDay = day.AddDate(0, 0, 1)
	}
	end := atClock(endDay, w.End)
	if !end.After(now) {
		return Instant{}, fmt.Errorf("time %q: that day's %s window: %w", spec, w, ErrPast)
	}
	start = later(start, now)
	span := end.Sub(start)
	if span <= time.Second {
		return Instant{Time: start.Truncate(time.Second)}, nil
	}
	return Instant{Time: start.Add(time.Duration(r.Int64N(int64(span)))).Truncate(time.Second)}, nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

var offsetRe = regexp.MustCompile(`^[+-]\d{2}(:?\d{2})?$`)

// splitZone takes a trailing time zone off a time.
func splitZone(raw string) (string, *time.Location) {
	f := strings.Fields(raw)
	if len(f) < 2 {
		return raw, nil
	}
	if loc := zone(f[len(f)-1]); loc != nil {
		return strings.Join(f[:len(f)-1], " "), loc
	}
	return raw, nil
}

// zone reads "UTC", "Z", "+01:00", "-0500", an IANA name, or an upper-case
// abbreviation the time zone database knows.
func zone(s string) *time.Location {
	switch strings.ToUpper(s) {
	case "Z", "UTC", "GMT":
		return time.UTC
	}
	if offsetRe.MatchString(s) {
		digits := strings.ReplaceAll(s[1:], ":", "")
		h, _ := strconv.Atoi(digits[:2])
		m := 0
		if len(digits) == 4 {
			m, _ = strconv.Atoi(digits[2:])
		}
		sec := h*3600 + m*60
		if s[0] == '-' {
			sec = -sec
		}
		return time.FixedZone(s, sec)
	}
	if strings.Contains(s, "/") || (len(s) >= 3 && s == strings.ToUpper(s) && s != strings.ToLower(s)) {
		if loc, err := time.LoadLocation(s); err == nil && s != "Local" {
			return loc
		}
	}
	return nil
}

// ParseInstant is Parse that also accepts exact times in the past. It pins
// a clock (NIGHTSHIFT_NOW) rather than scheduling anything.
func ParseInstant(spec string, now time.Time, w Window, r *rand.Rand) (time.Time, error) {
	raw := strings.TrimSpace(spec)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	for _, layout := range absoluteLayouts {
		if t, err := time.ParseInLocation(layout, raw, now.Location()); err == nil {
			return t, nil
		}
	}
	return Parse(spec, now, w, r)
}

// ParseClock parses a time of day: "02:30", "2:30:15", "2am", "11:45pm".
func ParseClock(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	meridiem := ""
	if strings.HasSuffix(s, "am") || strings.HasSuffix(s, "pm") {
		meridiem = s[len(s)-2:]
		s = strings.TrimSpace(s[:len(s)-2])
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 || (len(parts) == 1 && meridiem == "") {
		return 0, fmt.Errorf("time of day %q: want HH:MM or 2am", s+meridiem)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" || (i > 0 && len(p) != 2) {
			return 0, fmt.Errorf("time of day %q: want HH:MM or 2am", s+meridiem)
		}
		n[i] = v
	}
	h, m, sec := n[0], n[1], n[2]
	if meridiem != "" {
		if h < 1 || h > 12 {
			return 0, fmt.Errorf("time of day %q: hour must be 1-12 with am/pm", s+meridiem)
		}
		switch {
		case meridiem == "am" && h == 12:
			h = 0
		case meridiem == "pm" && h != 12:
			h += 12
		}
	}
	if h > 23 || m > 59 || sec > 59 {
		return 0, fmt.Errorf("time of day %q is out of range", s+meridiem)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, nil
}

// FormatClock formats an offset from midnight as HH:MM.
func FormatClock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// atClock is day's local midnight plus off, built with time.Date so that a
// daylight saving change on that day lands on the wall-clock time asked for.
func atClock(day time.Time, off time.Duration) time.Time {
	h := int(off / time.Hour)
	m := int(off % time.Hour / time.Minute)
	s := int(off % time.Minute / time.Second)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, s, 0, day.Location())
}

func cutAny(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return s, false
}
