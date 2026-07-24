package domain

import (
	"time"
)

// AgeBand classifies a subject into a regulatory age band that determines which
// anti-addiction quota applies.
type AgeBand string

const (
	// BandChild is a child (< TeenMinAge).
	BandChild AgeBand = "child"
	// BandTeen is a teenager ([TeenMinAge, AdultMinAge)).
	BandTeen AgeBand = "teen"
	// BandAdult is an adult (>= AdultMinAge).
	BandAdult AgeBand = "adult"
)

// Regulatory age thresholds (years).
const (
	TeenMinAge  = 12
	AdultMinAge = 18
)

// AgeInLocation computes a subject's age in whole years as observed at instant
// `at`, using the subject's local timezone. Both the birth date and `at` are
// projected into loc so that a subject "turns older" at local midnight of their
// birthday rather than at some UTC boundary — this is the timezone-dynamic age
// the spec requires.
func AgeInLocation(birth time.Time, at time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	b := birth.In(loc)
	now := at.In(loc)
	years := now.Year() - b.Year()
	// Subtract one if the birthday has not yet occurred this year.
	if now.Month() < b.Month() || (now.Month() == b.Month() && now.Day() < b.Day()) {
		years--
	}
	if years < 0 {
		years = 0
	}
	return years
}

// BandFor maps an age in years to its regulatory band.
func BandFor(ageYears int) AgeBand {
	switch {
	case ageYears < TeenMinAge:
		return BandChild
	case ageYears < AdultMinAge:
		return BandTeen
	default:
		return BandAdult
	}
}

// Policy encodes the quota rules for a band. A zero-valued limit means "no
// limit for this dimension".
type Policy struct {
	Band AgeBand
	// DailyLimit caps total engaged focus time per local calendar day.
	DailyLimit time.Duration
	// SessionLimit caps a single continuous focus session.
	SessionLimit time.Duration
	// CurfewStart and CurfewEnd, when non-nil, define a nightly window
	// [start,end) in local wall-clock minutes-from-midnight during which focus
	// is forbidden ("睡前禁刷"). A window that wraps past midnight is supported.
	CurfewStart *int
	CurfewEnd   *int
}

// Curfew helpers express minutes-from-midnight.
func minutesPtr(m int) *int { return &m }

// PolicyFor returns the anti-addiction policy for a band. The rules are:
//
//   - Adult (成年人): 每日 60 分钟，单次 15 分钟。
//   - Teen  (青少年): 每日 30 分钟，睡前 1 小时禁刷 (22:00–23:00 local).
//   - Child (儿童):   单次 10 分钟。
//
// "睡前 1 小时" is interpreted as the hour ending at the 23:00 local bedtime,
// i.e. the curfew window [22:00, 23:00). This is centralised here so the fold
// and any explanation code share a single source of truth.
func PolicyFor(band AgeBand) Policy {
	switch band {
	case BandAdult:
		return Policy{
			Band:         BandAdult,
			DailyLimit:   60 * time.Minute,
			SessionLimit: 15 * time.Minute,
		}
	case BandTeen:
		return Policy{
			Band:        BandTeen,
			DailyLimit:  30 * time.Minute,
			CurfewStart: minutesPtr(22 * 60),
			CurfewEnd:   minutesPtr(23 * 60),
		}
	case BandChild:
		return Policy{
			Band:         BandChild,
			SessionLimit: 10 * time.Minute,
		}
	default:
		return Policy{Band: band}
	}
}

// inCurfew reports whether a local time-of-day (minutes from midnight) falls in
// the policy's curfew window. Supports windows that wrap past midnight.
func (p Policy) inCurfew(minutesOfDay int) bool {
	if p.CurfewStart == nil || p.CurfewEnd == nil {
		return false
	}
	start, end := *p.CurfewStart, *p.CurfewEnd
	if start <= end {
		return minutesOfDay >= start && minutesOfDay < end
	}
	// Wrapping window, e.g. [23:00, 06:00).
	return minutesOfDay >= start || minutesOfDay < end
}
