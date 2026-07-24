// Package domain contains the pure, side-effect-free core of the focus
// experiment service: the event model, canonical ordering, the idempotent
// order-independent fold that turns a bag of events into a replayable result,
// timezone-aware age computation and the anti-addiction quota policies.
//
// Nothing in this package performs IO. Every exported function is a pure
// function of its inputs, which is what makes results deterministic and
// replayable regardless of the order, duplication or lateness of event
// arrival.
package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"time"

	"github.com/google/uuid"
)

// EventType enumerates the four kinds of focus events the service ingests.
type EventType string

const (
	// EventCardView is a card being shown/browsed (卡片浏览).
	EventCardView EventType = "card_view"
	// EventAttentionSwitch marks the user switching attention away (注意切换).
	// It carries no engaged duration and acts as a session boundary.
	EventAttentionSwitch EventType = "attention_switch"
	// EventSlowReadAnswer is a slow-reading comprehension answer (慢阅读答题).
	EventSlowReadAnswer EventType = "slow_read_answer"
	// EventWatchSession is a media watching session (观看会话).
	EventWatchSession EventType = "watch_session"
)

// Valid reports whether t is one of the known event types.
func (t EventType) Valid() bool {
	switch t {
	case EventCardView, EventAttentionSwitch, EventSlowReadAnswer, EventWatchSession:
		return true
	default:
		return false
	}
}

// isBoundary reports whether the event type forcibly ends the current session.
func (t EventType) isBoundary() bool { return t == EventAttentionSwitch }

// Event is a single client-reported focus event. It is the immutable unit of
// input to the fold. OccurredAt is an absolute instant (stored as UTC); the
// local calendar day and time-of-day are derived using the subject's timezone.
//
// Idempotency: two events are the "same" event iff they share the same
// (ExperimentID, SubjectID, DeviceID, ClientSeq). A well-behaved client emits
// a strictly increasing ClientSeq per device, so this tuple uniquely names an
// event even across retries and cross-device concurrent uploads.
type Event struct {
	ExperimentID uuid.UUID     `json:"experiment_id"`
	SubjectID    uuid.UUID     `json:"subject_id"`
	DeviceID     string        `json:"device_id"`
	ClientSeq    int64         `json:"client_seq"`
	Type         EventType     `json:"type"`
	OccurredAt   time.Time     `json:"occurred_at"`
	Duration     time.Duration `json:"-"`
	// DurationMS mirrors Duration for JSON transport.
	DurationMS int64 `json:"duration_ms"`
}

// Key is the natural idempotency key of an event.
type Key struct {
	ExperimentID uuid.UUID
	SubjectID    uuid.UUID
	DeviceID     string
	ClientSeq    int64
}

// Key returns the event's idempotency key.
func (e Event) Key() Key {
	return Key{e.ExperimentID, e.SubjectID, e.DeviceID, e.ClientSeq}
}

// normalize reconciles the Duration and DurationMS representations, preferring
// Duration when set and otherwise deriving it from DurationMS.
func (e Event) normalize() Event {
	if e.Duration == 0 && e.DurationMS != 0 {
		e.Duration = time.Duration(e.DurationMS) * time.Millisecond
	}
	e.DurationMS = e.Duration.Milliseconds()
	e.OccurredAt = e.OccurredAt.UTC()
	return e
}

// payloadBytes returns a stable byte encoding of the fields that define the
// event's content (everything except the pure identity tuple), used both for
// the replay digest and for detecting conflicting duplicates.
func (e Event) payloadBytes() []byte {
	b := make([]byte, 0, 64)
	b = append(b, e.Type...)
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(e.OccurredAt.UnixNano()))
	b = append(b, t[:]...)
	binary.BigEndian.PutUint64(t[:], uint64(e.Duration.Nanoseconds()))
	b = append(b, t[:]...)
	return b
}

// Less defines the total canonical ordering used by the fold. It sorts by
// occurrence time, then device, then client sequence, then type. Because the
// ordering is total and derived only from event content, folding the same set
// of events always visits them in the same order — the foundation of replay
// determinism.
func Less(a, b Event) bool {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		return a.OccurredAt.Before(b.OccurredAt)
	}
	if a.DeviceID != b.DeviceID {
		return a.DeviceID < b.DeviceID
	}
	if a.ClientSeq != b.ClientSeq {
		return a.ClientSeq < b.ClientSeq
	}
	return a.Type < b.Type
}

// Canonicalize deduplicates a slice of events by idempotency key and returns
// them in canonical order. When two events share a key but differ in payload
// (a conflicting duplicate), the one that sorts first is kept deterministically
// and the count of such conflicts is returned so callers can surface data
// quality issues without affecting determinism.
func Canonicalize(events []Event) (canonical []Event, conflicts int) {
	byKey := make(map[Key]Event, len(events))
	seenPayload := make(map[Key][]byte, len(events))
	for _, raw := range events {
		e := raw.normalize()
		k := e.Key()
		prev, ok := byKey[k]
		if !ok {
			byKey[k] = e
			seenPayload[k] = e.payloadBytes()
			continue
		}
		if !equalBytes(seenPayload[k], e.payloadBytes()) {
			conflicts++
		}
		// Deterministic tie-break: keep the event that sorts first.
		if Less(e, prev) {
			byKey[k] = e
			seenPayload[k] = e.payloadBytes()
		}
	}
	canonical = make([]Event, 0, len(byKey))
	for _, e := range byKey {
		canonical = append(canonical, e)
	}
	sort.Slice(canonical, func(i, j int) bool { return Less(canonical[i], canonical[j]) })
	return canonical, conflicts
}

// Digest returns a stable SHA-256 hex digest over the canonical event set. Two
// event bags that canonicalize to the same set produce the same digest, which
// lets the service detect whether a recomputation would change the result and
// lets tests assert replay consistency across permutations and duplication.
func Digest(events []Event) string {
	canonical, _ := Canonicalize(events)
	h := sha256.New()
	var buf [8]byte
	for _, e := range canonical {
		h.Write([]byte(e.ExperimentID.String()))
		h.Write([]byte(e.SubjectID.String()))
		h.Write([]byte(e.DeviceID))
		binary.BigEndian.PutUint64(buf[:], uint64(e.ClientSeq))
		h.Write(buf[:])
		h.Write(e.payloadBytes())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
