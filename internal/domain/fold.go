package domain

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// Subject describes the person an experiment result is computed for. Only the
// data needed by the fold lives here; personal identifiers are handled by the
// storage/crypto layers, never by the pure domain.
type Subject struct {
	ID       uuid.UUID
	Birth    time.Time
	Timezone string // IANA name, e.g. "Asia/Shanghai". Empty means UTC.
}

// location resolves the subject's timezone, defaulting to UTC on any failure so
// the fold never errors on bad configuration.
func (s Subject) location() *time.Location {
	if s.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// DailyUsage is the per-local-day rollup of engaged focus time and how much of
// it was allowed under the daily cap.
type DailyUsage struct {
	Day             string        `json:"day"` // local calendar day, YYYY-MM-DD
	Engaged         time.Duration `json:"-"`
	EngagedMS       int64         `json:"engaged_ms"`
	Allowed         time.Duration `json:"-"`
	AllowedMS       int64         `json:"allowed_ms"`
	OverDailyLimit  bool          `json:"over_daily_limit"`
	CurfewBlockedMS int64         `json:"curfew_blocked_ms"`
}

// Violation is a non-diagnostic record that a policy rule was exceeded. It
// intentionally carries no personal data, no raw device identifiers and no free
// text — only a machine code, the local day and the offending magnitude — so
// that results can be surfaced or exported without leaking who the subject is
// or reconstructing their behaviour.
type Violation struct {
	Code    string `json:"code"`
	Day     string `json:"day,omitempty"`
	AmountMS int64 `json:"amount_ms,omitempty"`
	LimitMS  int64 `json:"limit_ms,omitempty"`
}

// Violation codes.
const (
	ViolationDailyLimit   = "daily_limit_exceeded"
	ViolationSessionLimit = "session_limit_exceeded"
	ViolationCurfew       = "curfew_blocked"
)

// Result is the deterministic, replayable output of folding a subject's events
// under their applicable policy. Two runs over event bags with the same
// canonical content always produce an equal Result (verified by property
// tests), and Digest identifies that content.
type Result struct {
	ExperimentID   uuid.UUID     `json:"experiment_id"`
	SubjectID      uuid.UUID     `json:"subject_id"`
	AgeYears       int           `json:"age_years"`
	Band           AgeBand       `json:"band"`
	Timezone       string        `json:"timezone"`
	EventCount     int           `json:"event_count"`
	ConflictCount  int           `json:"conflict_count"`
	TotalEngaged   time.Duration `json:"-"`
	TotalEngagedMS int64         `json:"total_engaged_ms"`
	TotalAllowedMS int64         `json:"total_allowed_ms"`
	Daily          []DailyUsage  `json:"daily"`
	Violations     []Violation   `json:"violations"`
	Digest         string        `json:"digest"`
}

// FoldConfig lets callers reference the policy computation instant. AsOf is the
// instant used to compute the subject's age; it defaults to time.Now when zero.
type FoldConfig struct {
	AsOf time.Time
}

// session is internal fold state tracking a continuous run of engagement on one
// device, used to enforce the single-session limit and to attribute curfew.
type session struct {
	deviceID string
	start    time.Time
	end      time.Time
	engaged  time.Duration
}

// Fold is the idempotent, order-independent core computation. It:
//
//  1. canonicalises + deduplicates the events (so duplicates and reordering do
//     not change the outcome),
//  2. computes the subject's age in their timezone as of cfg.AsOf and selects
//     the matching anti-addiction policy,
//  3. walks events in canonical order, splitting them into sessions at
//     attention-switch boundaries and device changes,
//  4. accumulates engaged time per local calendar day, applying the daily cap,
//     single-session cap and nightly curfew,
//  5. emits a Result whose Digest identifies the canonical event content.
//
// Fold is a pure function: same inputs → identical Result.
func Fold(subject Subject, events []Event, cfg FoldConfig) Result {
	asOf := cfg.AsOf
	if asOf.IsZero() {
		asOf = time.Now()
	}
	loc := subject.location()
	age := AgeInLocation(subject.Birth, asOf, loc)
	band := BandFor(age)
	policy := PolicyFor(band)

	canonical, conflicts := Canonicalize(events)

	res := Result{
		ExperimentID:  subject.experimentID(canonical),
		SubjectID:     subject.ID,
		AgeYears:      age,
		Band:          band,
		Timezone:      tzName(subject.Timezone),
		EventCount:    len(canonical),
		ConflictCount: conflicts,
		Digest:        Digest(canonical),
	}

	// Per-day accumulators keyed by local calendar day.
	type dayAcc struct {
		engaged      time.Duration
		curfewBlocked time.Duration
	}
	days := map[string]*dayAcc{}
	dayOrder := []string{}
	getDay := func(d string) *dayAcc {
		a, ok := days[d]
		if !ok {
			a = &dayAcc{}
			days[d] = a
			dayOrder = append(dayOrder, d)
		}
		return a
	}

	var sessionViolationDays = map[string]bool{}
	var cur *session

	flush := func() {
		if cur == nil {
			return
		}
		// Single-session cap.
		if policy.SessionLimit > 0 && cur.engaged > policy.SessionLimit {
			day := localDay(cur.start, loc)
			if !sessionViolationDays[day+"|"+cur.deviceID+"|"+cur.start.String()] {
				res.Violations = append(res.Violations, Violation{
					Code:     ViolationSessionLimit,
					Day:      day,
					AmountMS: cur.engaged.Milliseconds(),
					LimitMS:  policy.SessionLimit.Milliseconds(),
				})
				sessionViolationDays[day+"|"+cur.deviceID+"|"+cur.start.String()] = true
			}
		}
		cur = nil
	}

	for _, e := range canonical {
		if e.Type.isBoundary() {
			flush()
			continue
		}
		day := localDay(e.OccurredAt, loc)
		acc := getDay(day)

		// Curfew: engagement whose start falls in the curfew window is blocked
		// entirely and does not accrue engaged/allowed time.
		if policy.inCurfew(minutesOfDay(e.OccurredAt, loc)) {
			acc.curfewBlocked += e.Duration
			flush() // curfew breaks any running session
			res.Violations = append(res.Violations, Violation{
				Code:     ViolationCurfew,
				Day:      day,
				AmountMS: e.Duration.Milliseconds(),
			})
			continue
		}

		acc.engaged += e.Duration

		// Session tracking: continue current session unless the device changed.
		if cur == nil || cur.deviceID != e.DeviceID {
			flush()
			cur = &session{deviceID: e.DeviceID, start: e.OccurredAt}
		}
		cur.end = e.OccurredAt.Add(e.Duration)
		cur.engaged += e.Duration
	}
	flush()

	// Build per-day usage applying the daily cap, in local-day order.
	sort.Strings(dayOrder)
	for _, d := range dayOrder {
		acc := days[d]
		allowed := acc.engaged
		over := false
		if policy.DailyLimit > 0 && allowed > policy.DailyLimit {
			allowed = policy.DailyLimit
			over = true
			res.Violations = append(res.Violations, Violation{
				Code:     ViolationDailyLimit,
				Day:      d,
				AmountMS: acc.engaged.Milliseconds(),
				LimitMS:  policy.DailyLimit.Milliseconds(),
			})
		}
		du := DailyUsage{
			Day:             d,
			Engaged:         acc.engaged,
			EngagedMS:       acc.engaged.Milliseconds(),
			Allowed:         allowed,
			AllowedMS:       allowed.Milliseconds(),
			OverDailyLimit:  over,
			CurfewBlockedMS: acc.curfewBlocked.Milliseconds(),
		}
		res.Daily = append(res.Daily, du)
		res.TotalEngaged += acc.engaged
		res.TotalAllowedMS += allowed.Milliseconds()
	}
	res.TotalEngagedMS = res.TotalEngaged.Milliseconds()

	// Deterministic violation ordering: by day, then code, then amount.
	sort.SliceStable(res.Violations, func(i, j int) bool {
		a, b := res.Violations[i], res.Violations[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.AmountMS < b.AmountMS
	})
	return res
}

// experimentID recovers the experiment id from the canonical events; falls back
// to zero when there are none (the subject id still identifies the result).
func (s Subject) experimentID(events []Event) uuid.UUID {
	if len(events) > 0 {
		return events[0].ExperimentID
	}
	return uuid.Nil
}

func tzName(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

// localDay formats an instant as its local calendar day in loc.
func localDay(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}

// minutesOfDay returns the wall-clock minutes-from-midnight of an instant in loc.
func minutesOfDay(t time.Time, loc *time.Location) int {
	lt := t.In(loc)
	return lt.Hour()*60 + lt.Minute()
}
