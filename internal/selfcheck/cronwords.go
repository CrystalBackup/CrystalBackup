/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package selfcheck

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
)

// cronInWords turns a cron expression into English, and it is deliberately allowed to give up.
//
// # Why it may give up, and why that is safe
//
// A complete cron-to-English translator is a surprising amount of code with a long tail of expressions
// nobody writes, and every one of its edge cases is a chance to tell an administrator confidently that
// their backup runs at a time it does not. That is a worse outcome than saying nothing: a wrong
// sentence is believed, an absent one is looked up.
//
// So this handles the shapes that actually appear in backup schedules — the macros, a fixed time of
// day, a fixed time on given weekdays or a day of the month, and the step forms — and returns "" for
// anything else. The empty string is not a failure the caller has to handle specially, because the
// sentence is never the only thing shown: cronNext computes the actual next occurrence from the same
// expression with the real parser, and an unambiguous timestamp is what an administrator needs most.
// The words are the convenience; the timestamp is the answer.
func cronInWords(expr, tz string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return ""
	}
	if w := cronMacroInWords(expr); w != "" {
		return w + tzSuffix(tz)
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		// Six-field (seconds) and other dialects: internal/schedule parses the CronJob dialect only,
		// so an expression of another shape is already reported as invalid elsewhere. Nothing to say.
		return ""
	}
	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// Anything restricting the month is out of scope: a quarterly backup window is rare enough that
	// the computed next occurrence serves better than a sentence about it.
	if month != "*" {
		return ""
	}

	// Step forms first: they describe a FREQUENCY rather than a time of day, and reading them as a
	// time of day is the mistake that produces a confidently wrong sentence.
	if n, ok := stepValue(minute); ok && hour == "*" && dom == "*" && dow == "*" {
		return fmt.Sprintf("every %s", plural(n, "minute")) + tzSuffix(tz)
	}
	if n, ok := stepValue(hour); ok && dom == "*" && dow == "*" {
		at, ok := minuteOnly(minute)
		if !ok {
			return ""
		}
		return fmt.Sprintf("every %s, at %s past the hour", plural(n, "hour"), plural(at, "minute")) +
			tzSuffix(tz)
	}

	times, ok := timesOfDay(minute, hour)
	if !ok {
		return ""
	}
	when := "at " + joinAnd(times)

	switch {
	case dom == "*" && dow == "*":
		return "every day " + when + tzSuffix(tz)
	case dom == "*":
		days, ok := weekdayNames(dow)
		if !ok {
			return ""
		}
		return "every " + joinAnd(days) + " " + when + tzSuffix(tz)
	case dow == "*":
		day, ok := singleNumber(dom)
		if !ok {
			return ""
		}
		return fmt.Sprintf("on day %d of every month ", day) + when + tzSuffix(tz)
	default:
		// Both day-of-month and day-of-week restricted. Cron's OR semantics between the two are a
		// classic source of misunderstanding, and explaining them in a backup report is not this
		// function's job.
		return ""
	}
}

// cronMacroInWords handles the @-macros internal/schedule accepts through cron.ParseStandard. They
// are spelled out rather than passed through because "@weekly" tells a reader the frequency and hides
// the fact that it lands at midnight on a Sunday, which is exactly the sort of thing somebody wants
// to know before it collides with their prune window.
func cronMacroInWords(expr string) string {
	switch strings.ToLower(expr) {
	case "@yearly", "@annually":
		return "once a year, on 1 January at 00:00"
	case "@monthly":
		return "on the 1st of every month at 00:00"
	case "@weekly":
		return "every Sunday at 00:00"
	case "@daily", "@midnight":
		return "every day at 00:00"
	case "@hourly":
		return "every hour, on the hour"
	default:
		if rest, ok := strings.CutPrefix(strings.ToLower(expr), "@every "); ok {
			if d, err := time.ParseDuration(rest); err == nil {
				return "every " + d.String()
			}
		}
		return ""
	}
}

// timesOfDay renders a (minute, hour) pair as a list of HH:MM strings, and refuses anything it cannot
// render exactly. Lists are supported on both halves because "0 2,3 * * *" is a real expression with a
// real trap in it — its two activations are one hour and twenty-three hours apart — and a report that
// showed only one of them would be hiding the trap.
func timesOfDay(minute, hour string) ([]string, bool) {
	minutes, ok := numberList(minute, 0, 59)
	if !ok {
		return nil, false
	}
	hours, ok := numberList(hour, 0, 23)
	if !ok {
		return nil, false
	}
	// A cross product of two lists grows fast and stops being a sentence; past a handful the computed
	// next occurrence is the better answer.
	if len(minutes)*len(hours) > 4 {
		return nil, false
	}
	out := make([]string, 0, len(minutes)*len(hours))
	for _, h := range hours {
		for _, m := range minutes {
			out = append(out, fmt.Sprintf("%02d:%02d", h, m))
		}
	}
	return out, true
}

// numberList parses "5", "2,3" or "0" into values, rejecting ranges, steps and wildcards — every one
// of which this function's caller has already decided it will not try to put into words.
func numberList(field string, min, max int) ([]int, bool) {
	if field == "*" || field == "" {
		return nil, false
	}
	parts := strings.Split(field, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < min || n > max {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func singleNumber(field string) (int, bool) {
	list, ok := numberList(field, 1, 31)
	if !ok || len(list) != 1 {
		return 0, false
	}
	return list[0], true
}

func minuteOnly(field string) (int, bool) {
	list, ok := numberList(field, 0, 59)
	if !ok || len(list) != 1 {
		return 0, false
	}
	return list[0], true
}

// stepValue reads "*/n" and returns n.
func stepValue(field string) (int, bool) {
	rest, ok := strings.CutPrefix(field, "*/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// cronWeekdays is indexed by cron's own day-of-week numbering, in which both 0 and 7 are Sunday.
var cronWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// cronWeekdayNames maps the three-letter names cron accepts. Included because the chart's examples and
// most hand-written schedules use them, and a report that could not name "SUN" would fall back to
// silence on the commonest weekly expression there is.
var cronWeekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// weekdayNames renders a day-of-week field as names. It handles a list and a hyphenated range,
// because "1-5" (weekdays) is the second most common shape after a single day.
func weekdayNames(field string) ([]string, bool) {
	var out []string
	for part := range strings.SplitSeq(field, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, aok := weekdayIndex(lo)
			b, bok := weekdayIndex(hi)
			if !aok || !bok || a > b {
				return nil, false
			}
			for i := a; i <= b; i++ {
				out = append(out, cronWeekdays[i])
			}
			continue
		}
		i, ok := weekdayIndex(part)
		if !ok {
			return nil, false
		}
		out = append(out, cronWeekdays[i])
	}
	if len(out) == 0 || len(out) > 7 {
		return nil, false
	}
	return out, true
}

func weekdayIndex(s string) (int, bool) {
	if i, ok := cronWeekdayNames[strings.ToLower(strings.TrimSpace(s))]; ok {
		return i, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 7 {
		return 0, false
	}
	// 7 and 0 are both Sunday in cron's numbering.
	return n % 7, true
}

// cronNext is the first activation strictly after now, computed with internal/schedule — the SAME
// parser the schedule controller fires on, which is the only reason this number can be trusted as a
// prediction rather than offered as an estimate. Nil when the expression does not parse, which the
// caller reports as its own problem rather than papering over with a guess.
func cronNext(expr, tz string, now time.Time) *time.Time {
	s, err := schedule.Parse(expr, tz)
	if err != nil {
		return nil
	}
	at := s.Next(now).UTC()
	return &at
}

// tzSuffix names the timezone the words are to be read in. It is never omitted for a schedule that
// declares one, and says UTC when none is declared, because an unqualified "at 03:00" in a document
// that will be read by somebody in another country is an invitation to schedule a prune window in the
// middle of their working day — which is the specific mistake MaintenanceSpec.Timezone's own
// documentation warns about.
func tzSuffix(tz string) string {
	if tz == "" {
		return " UTC"
	}
	return " " + tz
}

// plural renders "1 minute" / "5 minutes" without the (n) parenthetical that makes a generated
// sentence read like a form.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// joinAnd renders a short list as "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
