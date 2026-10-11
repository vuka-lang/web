// Package cron parses cron schedules and computes their next run: five
// fields (minute hour day-of-month month day-of-week), the descriptors
// @yearly, @monthly, @weekly, @daily, @hourly and @every <duration>, and a
// CRON_TZ= prefix naming the time zone the fields are read in.
//
//	s, err := cron.Parse("CRON_TZ=Africa/Dar_es_Salaam 0 7 * * 1-5")
//	next := s.Next(time.Now()) // 07:00 in Dar es Salaam, the next weekday
//
// web schedules @web.Cron functions with it; it has no dependencies.
package cron

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Schedule says when a job runs next.
type Schedule interface {
	// Next is the first run strictly after t; the zero time when there is
	// none.
	Next(t time.Time) time.Time
}

// Parse reads a schedule:
//
//	*/5 * * * *                 every five minutes
//	0 7 * * 1-5                 07:00 on weekdays
//	30 2 1,15 * *               02:30 on the 1st and the 15th
//	0 0 * JAN,JUL SUN           midnight on Sundays of January and July
//	@hourly, @daily (@midnight), @weekly, @monthly, @yearly (@annually)
//	@every 90s                  every 90 seconds (see Every)
//	CRON_TZ=Europe/Paris 0 9 * * *   the fields read in that zone
//
// A field is *, a value, a range a-b, a list a,b,c, and each with a step
// /n; months and days of the week also by name (JAN, MON), and Sunday is 0
// or 7. When both the day of the month and the day of the week are
// restricted, a day matching either runs (cron's rule). Without CRON_TZ
// (or TZ=) the fields are read in the time zone of the time Next is given.
func Parse(spec string) (Schedule, error) {
	s := strings.TrimSpace(spec)
	var loc *time.Location
	rest, tz := cutPrefixFold(s, "CRON_TZ=")
	if !tz {
		rest, tz = cutPrefixFold(s, "TZ=")
	}
	if tz {
		name, fields, _ := strings.Cut(rest, " ")
		l, err := time.LoadLocation(name)
		if err != nil || name == "" {
			return nil, fmt.Errorf("cron: %q: unknown time zone %q", spec, name)
		}
		loc, s = l, strings.TrimSpace(fields)
	}
	if strings.HasPrefix(s, "@") {
		name, arg, _ := strings.Cut(s, " ")
		switch strings.ToLower(name) {
		case "@every":
			if loc != nil {
				return nil, fmt.Errorf("cron: %q: @every takes no time zone", spec)
			}
			d, err := time.ParseDuration(strings.TrimSpace(arg))
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("cron: %q: @every needs a positive duration such as 30s or 1h30m", spec)
			}
			return Every(d), nil
		case "@yearly", "@annually":
			s = "0 0 1 1 *"
		case "@monthly":
			s = "0 0 1 * *"
		case "@weekly":
			s = "0 0 * * 0"
		case "@daily", "@midnight":
			s = "0 0 * * *"
		case "@hourly":
			s = "0 * * * *"
		default:
			return nil, fmt.Errorf("cron: %q: unknown descriptor %s", spec, name)
		}
		if strings.TrimSpace(arg) != "" {
			return nil, fmt.Errorf("cron: %q: %s takes nothing after it", spec, name)
		}
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron: %q: want five fields (minute hour day-of-month month day-of-week), have %d", spec, len(f))
	}
	sc := &spec5{loc: loc}
	var err error
	for i, fl := range fieldsOf {
		var star bool
		var set uint64
		if set, star, err = fl.parse(f[i]); err != nil {
			return nil, fmt.Errorf("cron: %q: %v", spec, err)
		}
		switch i {
		case 0:
			sc.minute = set
		case 1:
			sc.hour, sc.anyHour = set, star
		case 2:
			sc.dom, sc.anyDom = set, star
		case 3:
			sc.month = set
		case 4:
			if set&(1<<7) != 0 {
				set = set&^(1<<7) | 1
			}
			sc.dow, sc.anyDow = set, star
		}
	}
	if !sc.anyDom && sc.anyDow && !sc.possible() {
		return nil, fmt.Errorf("cron: %q: never runs: no month given has those days", spec)
	}
	return sc, nil
}

// MustParse is Parse, panicking on an error: for schedules written in code.
func MustParse(spec string) Schedule {
	s, err := Parse(spec)
	if err != nil {
		panic(err)
	}
	return s
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// Every runs every d, at the multiples of d counted from a fixed origin
// (time.Time's zero), so every process — every replica, every restart —
// agrees on the ticks: @every 30s runs at :00 and :30 of each minute.
type Every time.Duration

func (e Every) Next(t time.Time) time.Time {
	d := time.Duration(e)
	return t.Truncate(d).Add(d)
}

func (e Every) String() string { return "@every " + time.Duration(e).String() }

type field struct {
	name     string
	min, max int
	names    []string // names[i] is value min+i
}

var fieldsOf = []field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}},
	{name: "day of week", min: 0, max: 7, names: []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}},
}

// parse reads one field into a bit set; star says it is * (or ?), all values.
func (f field) parse(s string) (set uint64, star bool, err error) {
	for _, part := range strings.Split(s, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		lo, hi := f.min, f.max
		if f.name == "day of week" {
			hi = 6 // * is Sunday to Saturday; 7 only when written
		}
		switch {
		case rng == "*" || rng == "?":
			if !hasStep && len(s) == len(part) {
				star = true
			}
		case rng == "":
			return 0, false, fmt.Errorf("%s: empty value in %q", f.name, s)
		default:
			a, b, isRange := strings.Cut(rng, "-")
			if lo, err = f.value(a); err != nil {
				return 0, false, err
			}
			hi = lo
			if isRange {
				if hi, err = f.value(b); err != nil {
					return 0, false, err
				}
				if hi < lo {
					return 0, false, fmt.Errorf("%s: range %s runs backwards", f.name, rng)
				}
			} else if hasStep {
				hi = f.max // 5/15: from 5 to the end, as cron reads it
				if f.name == "day of week" {
					hi = 6
				}
			}
		}
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepText); err != nil || step <= 0 {
				return 0, false, fmt.Errorf("%s: step %q isn't a positive number", f.name, stepText)
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, star, nil
}

func (f field) value(s string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is neither a number nor a name", f.name, s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%s: %d is out of range %d-%d", f.name, v, f.min, f.max)
	}
	return v, nil
}

// spec5 is a five-field schedule: a bit set per field.
type spec5 struct {
	minute, hour, dom, month, dow uint64
	anyHour, anyDom, anyDow       bool
	loc                           *time.Location
}

// possible reports whether some month given has some day given (February
// counting its 29th).
func (s *spec5) possible() bool {
	for m := 1; m <= 12; m++ {
		if s.month&(1<<uint(m)) != 0 && s.dom&(1<<uint(daysIn(m, 2000)+1)-1) != 0 {
			return true
		}
	}
	return false
}

func daysIn(m, y int) int {
	return time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func (s *spec5) dayMatches(y, m, d int) bool {
	dom := s.dom&(1<<uint(d)) != 0
	dow := s.dow&(1<<uint(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC).Weekday())) != 0
	if s.anyDom || s.anyDow {
		return dom && dow
	}
	return dom || dow
}

// next finds the first value of set at or after v, below 64.
func next(set uint64, v int) (int, bool) {
	if v > 63 {
		return 0, false
	}
	rest := set >> uint(v) << uint(v)
	if rest == 0 {
		return 0, false
	}
	return bits.TrailingZeros64(rest), true
}

// Next walks the calendar in wall-clock time, field by field, then places
// the wall time found on the time line (when the hour field is *, it walks
// the time line itself: byClock):
//
//   - a wall time the clock skips (spring forward) runs at the instant of the
//     jump, once however many runs fell in the gap;
//   - a wall time the clock passes twice (fall back) runs once, at its first
//     passing — unless the hour field is *, when the job runs by the clock
//     both times, so an hourly or minutely job keeps its rhythm.
func (s *spec5) Next(t time.Time) time.Time {
	loc := s.loc
	if loc == nil {
		loc = t.Location()
	}
	if s.anyHour {
		return s.byClock(t, loc)
	}
	from := t
	t = t.In(loc)
	// The wall clock to search from: the minute after t's.
	w := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC).Add(time.Minute)
	y, mo, d, h, mi := w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute()
	for limit := y + 30; y <= limit; {
		m, ok := next(s.month, mo)
		if !ok || m > 12 {
			y, mo, d, h, mi = y+1, 1, 1, 0, 0
			continue
		}
		if m != mo {
			mo, d, h, mi = m, 1, 0, 0
		}
		for ; d <= daysIn(mo, y) && !s.dayMatches(y, mo, d); d++ {
			h, mi = 0, 0
		}
		if d > daysIn(mo, y) {
			mo, d, h, mi = mo+1, 1, 0, 0
			if mo > 12 {
				y, mo = y+1, 1
			}
			continue
		}
		hh, ok := next(s.hour, h)
		if !ok || hh > 23 {
			d, h, mi = d+1, 0, 0
			if d > daysIn(mo, y) {
				mo, d = mo+1, 1
				if mo > 12 {
					y, mo = y+1, 1
				}
			}
			continue
		}
		if hh != h {
			h, mi = hh, 0
		}
		mm, ok := next(s.minute, mi)
		if !ok || mm > 59 {
			h, mi = h+1, 0
			if h > 23 {
				d, h = d+1, 0
				if d > daysIn(mo, y) {
					mo, d = mo+1, 1
					if mo > 12 {
						y, mo = y+1, 1
					}
				}
			}
			continue
		}
		mi = mm
		if at, ok := s.place(y, mo, d, h, mi, loc, from); ok {
			return at
		}
		mi++ // in the gap the jump already ran, or before from: go on
		if mi > 59 {
			h, mi = h+1, 0
			if h > 23 {
				d, h = d+1, 0
				if d > daysIn(mo, y) {
					mo, d = mo+1, 1
					if mo > 12 {
						y, mo = y+1, 1
					}
				}
			}
		}
	}
	return time.Time{}
}

// byClock is Next when the hour field is *: whether an instant runs depends
// only on its date and minute, so it walks the time line itself — a hour
// the clock skips has no runs, one it repeats has them twice.
func (s *spec5) byClock(t time.Time, loc *time.Location) time.Time {
	u := t.Truncate(time.Minute).Add(time.Minute)
	for limit := u.Year() + 30; u.Year() <= limit; {
		l := u.In(loc)
		y, mo, d := l.Date()
		switch {
		case s.month&(1<<uint(mo)) == 0:
			u = time.Date(y, mo+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(y, int(mo), d):
			u = time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
		default:
			mm, ok := next(s.minute, l.Minute())
			if !ok || mm > 59 {
				u = u.Add(time.Duration(60-l.Minute()) * time.Minute)
				continue
			}
			return u.Add(time.Duration(mm-l.Minute()) * time.Minute).In(loc)
		}
	}
	return time.Time{}
}

// place puts a wall time on the time line: the instant it names, the first
// of two in a repeated hour, or the jump over it when the clock skips it;
// ok only after from.
func (s *spec5) place(y, mo, d, h, mi int, loc *time.Location, from time.Time) (time.Time, bool) {
	wall := time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC)
	// The offsets around the wall time: a zone changes its offset at most
	// once in a day.
	_, before := wall.Add(-14 * time.Hour).In(loc).Zone()
	_, after := wall.Add(14 * time.Hour).In(loc).Zone()
	var found []time.Time
	for _, off := range []int{before, after} {
		u := wall.Add(-time.Duration(off) * time.Second)
		l := u.In(loc)
		if l.Year() == y && int(l.Month()) == mo && l.Day() == d && l.Hour() == h && l.Minute() == mi {
			found = append(found, u.In(loc))
		}
		if before == after {
			break
		}
	}
	if len(found) == 0 {
		// Skipped: the jump is the first instant whose wall time is past it,
		// between the wall time read with the offset after the jump and
		// with the one before it.
		lo, hi := wall.Add(-time.Duration(after)*time.Second), wall.Add(-time.Duration(before)*time.Second)
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2)
			if l := mid.In(loc); time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC).Before(wall) {
				lo = mid
			} else {
				hi = mid
			}
		}
		jump := hi.Truncate(time.Second).In(loc)
		return jump, jump.After(from)
	}
	first := found[0]
	for _, u := range found[1:] {
		if u.Before(first) {
			first = u
		}
	}
	return first, first.After(from)
}

// Clock is the time a scheduler runs by: the real one, or a virtual one in
// tests (crontest.Clock).
type Clock interface {
	Now() time.Time
	// NewTimer fires once, d from now.
	NewTimer(d time.Duration) Timer
}

// Timer is a Clock's timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// RealClock is the system's clock.
var RealClock Clock = realClock{}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }
