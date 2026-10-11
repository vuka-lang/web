package cron

import (
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, loc, s string) time.Time {
	t.Helper()
	l, err := time.LoadLocation(loc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, l)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNext(t *testing.T) {
	const f = "2006-01-02 15:04:05 MST"
	for _, c := range []struct {
		spec, loc, from string
		want            []string // the runs after from, in order
	}{
		{"* * * * *", "UTC", "2024-01-01 10:00:30", []string{"2024-01-01 10:01:00 UTC", "2024-01-01 10:02:00 UTC"}},
		{"*/15 * * * *", "UTC", "2024-01-01 10:00:00", []string{"2024-01-01 10:15:00 UTC", "2024-01-01 10:30:00 UTC", "2024-01-01 10:45:00 UTC", "2024-01-01 11:00:00 UTC"}},
		{"5/20 * * * *", "UTC", "2024-01-01 10:00:00", []string{"2024-01-01 10:05:00 UTC", "2024-01-01 10:25:00 UTC", "2024-01-01 10:45:00 UTC", "2024-01-01 11:05:00 UTC"}},
		{"0 7 * * *", "UTC", "2024-01-01 07:00:00", []string{"2024-01-02 07:00:00 UTC", "2024-01-03 07:00:00 UTC"}},
		{"0 7 * * 1-5", "UTC", "2024-01-05 08:00:00", []string{"2024-01-08 07:00:00 UTC", "2024-01-09 07:00:00 UTC"}}, // Friday → Monday
		{"0 7 * * MON-FRI", "UTC", "2024-01-05 08:00:00", []string{"2024-01-08 07:00:00 UTC"}},
		{"0 0 * * 7", "UTC", "2024-01-01 00:00:00", []string{"2024-01-07 00:00:00 UTC", "2024-01-14 00:00:00 UTC"}}, // 7 is Sunday
		{"0 0 * * 5-7", "UTC", "2024-01-01 00:00:00", []string{"2024-01-05 00:00:00 UTC", "2024-01-06 00:00:00 UTC", "2024-01-07 00:00:00 UTC", "2024-01-12 00:00:00 UTC"}},
		{"30 2 1,15 * *", "UTC", "2024-01-02 00:00:00", []string{"2024-01-15 02:30:00 UTC", "2024-02-01 02:30:00 UTC"}},
		{"0 0 1 1 *", "UTC", "2024-06-01 00:00:00", []string{"2025-01-01 00:00:00 UTC", "2026-01-01 00:00:00 UTC"}},
		{"0 0 * JAN,jul SUN", "UTC", "2024-01-20 00:00:00", []string{"2024-01-21 00:00:00 UTC", "2024-01-28 00:00:00 UTC", "2024-07-07 00:00:00 UTC"}},
		{"0 12 29 2 *", "UTC", "2024-03-01 00:00:00", []string{"2028-02-29 12:00:00 UTC", "2032-02-29 12:00:00 UTC"}},
		{"0 0 31 * *", "UTC", "2024-01-31 00:00:00", []string{"2024-03-31 00:00:00 UTC", "2024-05-31 00:00:00 UTC"}},
		// Both days restricted: either matches.
		{"0 0 13 * FRI", "UTC", "2024-09-01 00:00:00", []string{"2024-09-06 00:00:00 UTC", "2024-09-13 00:00:00 UTC", "2024-09-20 00:00:00 UTC"}},
		{"0 9-17/4 * * *", "UTC", "2024-01-01 00:00:00", []string{"2024-01-01 09:00:00 UTC", "2024-01-01 13:00:00 UTC", "2024-01-01 17:00:00 UTC", "2024-01-02 09:00:00 UTC"}},
		{"59 23 31 12 *", "UTC", "2024-12-31 23:59:00", []string{"2025-12-31 23:59:00 UTC"}},
		{"@hourly", "UTC", "2024-01-01 10:59:59", []string{"2024-01-01 11:00:00 UTC", "2024-01-01 12:00:00 UTC"}},
		{"@daily", "UTC", "2024-01-01 10:00:00", []string{"2024-01-02 00:00:00 UTC"}},
		{"@midnight", "UTC", "2024-01-01 10:00:00", []string{"2024-01-02 00:00:00 UTC"}},
		{"@weekly", "UTC", "2024-01-01 10:00:00", []string{"2024-01-07 00:00:00 UTC"}},
		{"@monthly", "UTC", "2024-01-15 10:00:00", []string{"2024-02-01 00:00:00 UTC", "2024-03-01 00:00:00 UTC"}},
		{"@yearly", "UTC", "2024-01-15 10:00:00", []string{"2025-01-01 00:00:00 UTC"}},
		{"@annually", "UTC", "2024-01-15 10:00:00", []string{"2025-01-01 00:00:00 UTC"}},
		{"@every 30s", "UTC", "2024-01-01 10:00:10", []string{"2024-01-01 10:00:30 UTC", "2024-01-01 10:01:00 UTC"}},
		{"@every 1h", "UTC", "2024-01-01 10:00:00", []string{"2024-01-01 11:00:00 UTC", "2024-01-01 12:00:00 UTC"}},
		{"@every 1h", "Africa/Dar_es_Salaam", "2024-01-01 10:20:00", []string{"2024-01-01 11:00:00 EAT"}}, // whole hours of UTC
		// Without CRON_TZ the fields are read in the zone of the time given.
		{"0 7 * * *", "Africa/Dar_es_Salaam", "2024-01-01 08:00:00", []string{"2024-01-02 07:00:00 EAT"}},
		{"CRON_TZ=Africa/Dar_es_Salaam 0 7 * * 1-5", "UTC", "2024-01-05 05:00:00", []string{"2024-01-08 04:00:00 UTC", "2024-01-09 04:00:00 UTC"}},
		{"TZ=Asia/Kolkata 30 9 * * *", "UTC", "2024-01-01 00:00:00", []string{"2024-01-01 04:00:00 UTC"}},

		// DST, New York: 2024-03-10 02:00 EST jumps to 03:00 EDT;
		// 2024-11-03 02:00 EDT falls back to 01:00 EST.
		// A wall time the clock skips runs at the jump, once.
		{"CRON_TZ=America/New_York 30 2 * * *", "UTC", "2024-03-09 12:00:00", []string{"2024-03-10 07:00:00 UTC", "2024-03-11 06:30:00 UTC"}},
		{"CRON_TZ=America/New_York */15 2 * * *", "UTC", "2024-03-10 06:00:00", []string{"2024-03-10 07:00:00 UTC", "2024-03-11 06:00:00 UTC"}},
		{"CRON_TZ=America/New_York 0 3 * * *", "UTC", "2024-03-10 06:00:00", []string{"2024-03-10 07:00:00 UTC", "2024-03-11 07:00:00 UTC"}},
		// A wall time the clock passes twice runs once, at its first passing.
		{"CRON_TZ=America/New_York 30 1 * * *", "UTC", "2024-11-03 04:00:00", []string{"2024-11-03 05:30:00 UTC", "2024-11-04 06:30:00 UTC"}},
		{"CRON_TZ=America/New_York 30 1 * * *", "UTC", "2024-11-03 06:10:00", []string{"2024-11-04 06:30:00 UTC"}}, // from inside the second passing
		{"CRON_TZ=America/New_York 0 7 * * *", "UTC", "2024-11-02 12:00:00", []string{"2024-11-03 12:00:00 UTC", "2024-11-04 12:00:00 UTC"}},
		// With the hour field *, by the clock: skipped hours have no runs,
		// repeated ones run twice.
		{"CRON_TZ=America/New_York 0 * * * *", "UTC", "2024-03-10 05:30:00", []string{"2024-03-10 06:00:00 UTC", "2024-03-10 07:00:00 UTC", "2024-03-10 08:00:00 UTC"}},
		{"CRON_TZ=America/New_York 30 * * * *", "UTC", "2024-11-03 04:45:00", []string{"2024-11-03 05:30:00 UTC", "2024-11-03 06:30:00 UTC", "2024-11-03 07:30:00 UTC"}},
		{"CRON_TZ=America/New_York 0 * 3 11 *", "UTC", "2024-11-03 04:30:00", []string{"2024-11-03 05:00:00 UTC", "2024-11-03 06:00:00 UTC", "2024-11-03 07:00:00 UTC"}},
		// Europe: 2024-03-31 02:00 CET → 03:00 CEST; 2024-10-27 03:00 CEST → 02:00 CET.
		{"CRON_TZ=Europe/Berlin 15 2 * * *", "UTC", "2024-03-30 12:00:00", []string{"2024-03-31 01:00:00 UTC", "2024-04-01 00:15:00 UTC"}},
		{"CRON_TZ=Europe/Berlin 15 2 * * *", "UTC", "2024-10-26 12:00:00", []string{"2024-10-27 00:15:00 UTC", "2024-10-28 01:15:00 UTC"}},
		// A half-hour shift: Lord Howe goes from +10:30 to +11 at 02:00.
		{"CRON_TZ=Australia/Lord_Howe 15 2 * * *", "UTC", "2024-10-05 00:00:00", []string{"2024-10-05 15:30:00 UTC", "2024-10-06 15:15:00 UTC"}},
		// Zones without DST don't move.
		{"CRON_TZ=Africa/Dar_es_Salaam 0 0 * * *", "UTC", "2024-03-30 12:00:00", []string{"2024-03-30 21:00:00 UTC", "2024-03-31 21:00:00 UTC"}},
	} {
		s, err := Parse(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		from := at(t, c.loc, c.from)
		var got []string
		for range c.want {
			from = s.Next(from)
			got = append(got, from.In(time.UTC).Format(f))
			if strings.HasSuffix(c.want[0], "EAT") {
				got[len(got)-1] = from.Format(f)
			}
		}
		if strings.Join(got, ", ") != strings.Join(c.want, ", ") {
			t.Errorf("%s from %s %s:\n got %s\nwant %s", c.spec, c.from, c.loc, strings.Join(got, ", "), strings.Join(c.want, ", "))
		}
	}
}

func TestParseErrors(t *testing.T) {
	for spec, want := range map[string]string{
		"":                               "want five fields",
		"* * * *":                        "have 4",
		"* * * * * *":                    "have 6",
		"60 * * * *":                     "minute: 60 is out of range 0-59",
		"* 24 * * *":                     "hour: 24 is out of range 0-23",
		"* * 0 * *":                      "day of month: 0 is out of range 1-31",
		"* * * 13 *":                     "month: 13 is out of range 1-12",
		"* * * * 8":                      "day of week: 8 is out of range 0-7",
		"* * * FOO *":                    `month: "FOO" is neither a number nor a name`,
		"5-1 * * * *":                    "range 5-1 runs backwards",
		"*/0 * * * *":                    `step "0" isn't a positive number`,
		"*/x * * * *":                    `step "x"`,
		"1,,2 * * * *":                   "empty value",
		"0 0 30 2 *":                     "never runs",
		"0 0 31 4,6,9,11 *":              "never runs",
		"@every":                         "positive duration",
		"@every -1s":                     "positive duration",
		"@every soon":                    "positive duration",
		"@fortnightly":                   "unknown descriptor @fortnightly",
		"@daily 5":                       "@daily takes nothing after it",
		"CRON_TZ=Mars/Olympus 0 7 * * *": `unknown time zone "Mars/Olympus"`,
		"CRON_TZ=UTC @every 1m":          "@every takes no time zone",
	} {
		_, err := Parse(spec)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v; want %q", spec, err, want)
		}
	}
	// Possible with the day of week: either day matches.
	if _, err := Parse("0 0 30 2 MON"); err != nil {
		t.Error(err)
	}
}
