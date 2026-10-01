package discharge

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// TripReason explains why the flow interlock forced a discharge valve shut.
type TripReason string

const (
	TripFlowOverLimit  TripReason = "flow_over_limit"
	TripTelemetryStale TripReason = "telemetry_stale"
	TripTelemetryLost  TripReason = "telemetry_lost"
	TripPermitInactive TripReason = "permit_inactive"
)

// FlowConfig is the per-line, runtime-configurable protection envelope.
type FlowConfig struct {
	// MaxFlow is the configurable discharge flow upper limit. Reporting a
	// finite value above it trips the line immediately.
	MaxFlow float64
	// ContactTimeout is how long a line may keep discharging without a fresh
	// flow report (lost contact). Sweep enforces it on a periodic tick.
	ContactTimeout time.Duration
	// TelemetryTTL is the maximum age a reported ObservedAt may have. Older
	// telemetry cannot justify an open valve.
	TelemetryTTL time.Duration
}

func (c FlowConfig) normalized() (FlowConfig, error) {
	if math.IsNaN(c.MaxFlow) || math.IsInf(c.MaxFlow, 0) || c.MaxFlow <= 0 {
		return FlowConfig{}, errors.New("discharge flow limit must be positive")
	}
	if c.ContactTimeout <= 0 || c.TelemetryTTL <= 0 {
		return FlowConfig{}, errors.New("discharge contact timeout and telemetry ttl must be positive")
	}
	if c.ContactTimeout < c.TelemetryTTL {
		return FlowConfig{}, errors.New("discharge contact timeout must be no shorter than telemetry ttl")
	}
	c.MaxFlow = math.Round(c.MaxFlow*1000) / 1000
	c.ContactTimeout = c.ContactTimeout.Round(time.Second)
	c.TelemetryTTL = c.TelemetryTTL.Round(time.Second)
	return c, nil
}

// FlowReading is the actual discharge flow reported per process line.
type FlowReading struct {
	LineID     model.LineID `json:"line_id"`
	Value      float64      `json:"value"`
	ObservedAt time.Time    `json:"observed_at"`
	ReceivedAt time.Time    `json:"received_at"`
}

// TripRecord captures a single protective shutdown with its reason and the
// evidence that triggered it.
type TripRecord struct {
	ID         uuid.UUID    `json:"id"`
	LineID     model.LineID `json:"line_id"`
	Reason     TripReason   `json:"reason"`
	Message    string       `json:"message"`
	Flow       float64      `json:"flow,omitempty"`
	Limit      float64      `json:"limit,omitempty"`
	ObservedAt *time.Time   `json:"observed_at,omitempty"`
	PermitID   uuid.UUID    `json:"permit_id,omitempty"`
	TrippedAt  time.Time    `json:"tripped_at"`
}

// FlowState is the interlock state of one process line.
type FlowState struct {
	LineID     model.LineID `json:"line_id"`
	Config     ConfigView   `json:"config"`
	ValveOpen  bool         `json:"valve_open"`
	PermitID   uuid.UUID    `json:"permit_id,omitempty"`
	LatestFlow *FlowReading `json:"latest_flow,omitempty"`
	Tripped    bool         `json:"tripped"`
	Trip       *TripRecord  `json:"trip,omitempty"`
	LastTrip   *TripRecord  `json:"last_trip,omitempty"`
	ResetAt    *time.Time   `json:"reset_at,omitempty"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

// ConfigView carries the protection envelope with durations in seconds for
// API/JSON consumers.
type ConfigView struct {
	MaxFlow               float64 `json:"max_flow"`
	ContactTimeoutSeconds int64   `json:"contact_timeout_seconds"`
	TelemetryTTLSeconds   int64   `json:"telemetry_ttl_seconds"`
}

func (c FlowConfig) view() ConfigView {
	return ConfigView{
		MaxFlow:               c.MaxFlow,
		ContactTimeoutSeconds: int64(c.ContactTimeout / time.Second),
		TelemetryTTLSeconds:   int64(c.TelemetryTTL / time.Second),
	}
}

// TripNotifier is told, outside the interlock lock, whenever protection
// forces a valve shut, so the host can raise a line-scoped alarm.
type TripNotifier func(TripRecord)

type flowLine struct {
	config    FlowConfig
	latest    *FlowReading
	tripped   bool
	trip      *TripRecord
	lastTrip  *TripRecord
	resetAt   *time.Time
	updatedAt time.Time
	wasOpen   bool
	openSince time.Time
}

// FlowInterlock decides, per process line, whether a discharge valve may stay
// open based on the permit and the flow telemetry actually reported by that
// line. Every action is scoped to a single LineID; one line tripping never
// affects any other line.
type FlowInterlock struct {
	mu       sync.Mutex
	permits  *PermitBook
	valves   *ValveController
	defaults FlowConfig
	lines    map[model.LineID]*flowLine
	order    []model.LineID
	onTrip   TripNotifier
}

func NewFlowInterlock(permits *PermitBook, valves *ValveController, defaults FlowConfig, onTrip TripNotifier) (*FlowInterlock, error) {
	if permits == nil || valves == nil {
		return nil, errors.New("flow interlock requires permit book and valve controller")
	}
	defaults, err := defaults.normalized()
	if err != nil {
		return nil, err
	}
	if onTrip == nil {
		onTrip = func(TripRecord) {}
	}
	return &FlowInterlock{
		permits: permits, valves: valves, defaults: defaults,
		lines: make(map[model.LineID]*flowLine), onTrip: onTrip,
	}, nil
}

// EnableLine starts supervising a line with the default envelope. It is
// idempotent: enabling an already supervised line changes nothing; use
// Configure to adjust its limits.
func (f *FlowInterlock) EnableLine(lineID model.LineID, now time.Time) error {
	if lineID == "" {
		return errors.New("flow interlock line is required")
	}
	now = now.UTC()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.lines[lineID]; exists {
		return nil
	}
	f.lines[lineID] = &flowLine{config: f.defaults, updatedAt: now}
	f.order = append(f.order, lineID)
	sort.Slice(f.order, func(i, j int) bool { return f.order[i] < f.order[j] })
	return nil
}

// Configure changes the protection envelope of one line at runtime. It does
// not clear a latched trip; operators must Reset after adjusting limits.
func (f *FlowInterlock) Configure(lineID model.LineID, config FlowConfig, now time.Time) error {
	config, err := config.normalized()
	if err != nil {
		return err
	}
	now = now.UTC()
	f.mu.Lock()
	defer f.mu.Unlock()
	line, ok := f.lines[lineID]
	if !ok {
		return fmt.Errorf("flow interlock is not enabled for line %s", lineID)
	}
	line.config = config
	line.updatedAt = now
	return nil
}

// Report applies an actual flow reading from a process line and returns the
// resulting state. Fresh, in-limit readings with an active permit keep the
// valve open (and re-open it after a reset); violations force the valve shut
// and latch until Reset.
func (f *FlowInterlock) Report(reading FlowReading, now time.Time) (FlowState, error) {
	now = now.UTC()
	if reading.LineID == "" {
		return FlowState{}, errors.New("flow reading requires a process line")
	}
	if math.IsNaN(reading.Value) || math.IsInf(reading.Value, 0) || reading.Value < 0 {
		return FlowState{}, errors.New("flow reading must be a finite, non-negative value")
	}
	if reading.ObservedAt.Equal(time.Time{}) {
		return FlowState{}, errors.New("flow reading observed time is required")
	}
	reading.ObservedAt = reading.ObservedAt.UTC()

	f.mu.Lock()
	line, ok := f.lines[reading.LineID]
	if !ok {
		f.mu.Unlock()
		return FlowState{}, fmt.Errorf("flow interlock is not enabled for line %s", reading.LineID)
	}
	age := now.Sub(reading.ObservedAt)
	// Old telemetry must never justify an open valve, even on arrival.
	if age > line.config.TelemetryTTL {
		if f.valves.IsOpen(reading.LineID) {
			record := f.tripLocked(line, reading, TripTelemetryStale, fmt.Sprintf(
				"telemetry age %s exceeds ttl %s", age.Round(time.Second), line.config.TelemetryTTL), now)
			f.mu.Unlock()
			f.onTrip(record)
			return f.Snapshot(reading.LineID)
		}
		f.mu.Unlock()
		return FlowState{}, fmt.Errorf("flow telemetry is stale: age %s exceeds ttl %s",
			age.Round(time.Second), line.config.TelemetryTTL)
	}

	reading.ReceivedAt = now
	line.latest = &reading
	line.updatedAt = now

	if line.tripped {
		// Latch: fresh good data while tripped never auto-clears the trip.
		f.mu.Unlock()
		return f.Snapshot(reading.LineID)
	}

	var fired *TripRecord
	permit, permitActive := f.permits.ActiveForLine(reading.LineID)
	switch {
	case reading.Value > line.config.MaxFlow:
		record := f.tripLocked(line, reading, TripFlowOverLimit, fmt.Sprintf(
			"flow %.3f exceeds limit %.3f", reading.Value, line.config.MaxFlow), now)
		fired = &record
	case !permitActive:
		if f.valves.IsOpen(reading.LineID) {
			record := f.tripLocked(line, reading, TripPermitInactive,
				"discharge permit is no longer active", now)
			fired = &record
		}
		// Valve already closed (e.g. permit revoked): accept the reading, do
		// not re-open, do not raise a redundant protection record.
	default:
		if err := f.valves.Open(reading.LineID, permit.ID, now); err != nil {
			f.mu.Unlock()
			return FlowState{}, err
		}
		if !line.wasOpen {
			line.wasOpen = true
			line.openSince = now
		}
	}
	f.mu.Unlock()
	if fired != nil {
		f.onTrip(*fired)
	}
	return f.Snapshot(reading.LineID)
}

// Reset clears a latched trip after re-verifying the permit. The valve is not
// re-opened here: discharge resumes only when the next fresh, in-limit flow
// report arrives with an active permit.
func (f *FlowInterlock) Reset(lineID model.LineID, now time.Time) (FlowState, error) {
	now = now.UTC()
	f.mu.Lock()
	defer f.mu.Unlock()
	line, ok := f.lines[lineID]
	if !ok {
		return FlowState{}, fmt.Errorf("flow interlock is not enabled for line %s", lineID)
	}
	if _, active := f.permits.ActiveForLine(lineID); !active {
		return FlowState{}, errors.New("cannot reset flow interlock without an active discharge permit")
	}
	line.tripped = false
	line.trip = nil
	line.resetAt = &now
	line.updatedAt = now
	line.wasOpen = false
	line.openSince = time.Time{}
	// Re-verify before releasing the valve: keep it held until the next
	// Report proves telemetry is fresh and within limit.
	if err := f.valves.Hold(lineID, now); err != nil {
		return FlowState{}, err
	}
	return f.snapshotLocked(lineID, line), nil
}

// Sweep enforces time-based protection on every supervised line. It is meant
// to run on a periodic tick so that a line which stops reporting cannot keep
// discharging. Only open, untripped lines are evaluated.
func (f *FlowInterlock) Sweep(now time.Time) []FlowState {
	now = now.UTC()
	var records []TripRecord
	f.mu.Lock()
	for _, lineID := range f.order {
		line := f.lines[lineID]
		if line.tripped || !f.valves.IsOpen(lineID) {
			line.wasOpen = f.valves.IsOpen(lineID)
			continue
		}
		if !line.wasOpen {
			// Valve opened (e.g. by permit issuance) since the last sweep:
			// the contact timeout starts from the first observation.
			line.wasOpen = true
			line.openSince = now
		}
		// Continuously re-verify the permit even when telemetry is healthy.
		if _, active := f.permits.ActiveForLine(lineID); !active {
			reading := FlowReading{LineID: lineID, ReceivedAt: now}
			if line.latest != nil {
				reading = *line.latest
			}
			records = append(records, f.tripLocked(line, reading, TripPermitInactive,
				"discharge permit is no longer active", now))
			continue
		}
		if record := f.enforceTimeLocked(lineID, line, now); record != nil {
			records = append(records, *record)
		}
	}
	f.mu.Unlock()
	for _, record := range records {
		f.onTrip(record)
	}
	return f.All()
}

// enforceTimeLocked closes the valve when telemetry is too old or contact has
// been lost. Caller must hold f.mu. Returns the record when a trip occurred.
func (f *FlowInterlock) enforceTimeLocked(lineID model.LineID, line *flowLine, now time.Time) *TripRecord {
	if line.latest == nil {
		if idle := now.Sub(line.openSince); idle > line.config.ContactTimeout {
			record := f.tripLocked(line, FlowReading{LineID: lineID, ReceivedAt: now},
				TripTelemetryLost, fmt.Sprintf("no flow telemetry for %s after valve opened",
					idle.Round(time.Second)), now)
			return &record
		}
		return nil
	}
	age := now.Sub(line.latest.ObservedAt)
	silence := now.Sub(line.latest.ReceivedAt)
	switch {
	case silence > line.config.ContactTimeout:
		record := f.tripLocked(line, *line.latest, TripTelemetryLost, fmt.Sprintf(
			"flow telemetry lost: no report for %s", silence.Round(time.Second)), now)
		return &record
	case age > line.config.TelemetryTTL:
		record := f.tripLocked(line, *line.latest, TripTelemetryStale, fmt.Sprintf(
			"telemetry age %s exceeds ttl %s", age.Round(time.Second), line.config.TelemetryTTL), now)
		return &record
	}
	return nil
}

// tripLocked forces the valve shut and latches the reason. Caller must hold
// f.mu; the returned record is notified after unlock.
func (f *FlowInterlock) tripLocked(line *flowLine, reading FlowReading, reason TripReason, message string, now time.Time) TripRecord {
	lineID := reading.LineID
	permit, _ := f.permits.ActiveForLine(lineID)
	record := TripRecord{
		ID: uuid.New(), LineID: lineID, Reason: reason, Message: message,
		Flow: reading.Value, Limit: line.config.MaxFlow,
		TrippedAt: now, PermitID: permit.ID,
	}
	if !reading.ObservedAt.Equal(time.Time{}) {
		stamp := reading.ObservedAt
		record.ObservedAt = &stamp
	}
	line.tripped = true
	line.trip = &record
	line.lastTrip = &record
	line.updatedAt = now
	line.wasOpen = false
	line.openSince = time.Time{}
	// Only this line's valve is held; other lines are untouched.
	_ = f.valves.Hold(lineID, now)
	return record
}

func (f *FlowInterlock) Snapshot(lineID model.LineID) (FlowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line, ok := f.lines[lineID]
	if !ok {
		return FlowState{}, fmt.Errorf("flow interlock is not enabled for line %s", lineID)
	}
	return f.snapshotLocked(lineID, line), nil
}

// Caller must hold f.mu.
func (f *FlowInterlock) snapshotLocked(lineID model.LineID, line *flowLine) FlowState {
	state := FlowState{
		LineID:    lineID,
		Config:    line.config.view(),
		ValveOpen: f.valves.IsOpen(lineID),
		UpdatedAt: line.updatedAt,
	}
	if line.latest != nil {
		reading := *line.latest
		state.LatestFlow = &reading
	}
	if permit, active := f.permits.ActiveForLine(lineID); active {
		state.PermitID = permit.ID
	}
	if line.tripped {
		trip := *line.trip
		state.Tripped = true
		state.Trip = &trip
	}
	if line.lastTrip != nil {
		lastTrip := *line.lastTrip
		state.LastTrip = &lastTrip
	}
	if line.resetAt != nil {
		reset := *line.resetAt
		state.ResetAt = &reset
	}
	return state
}

// All returns the state of every supervised line, sorted by line id.
func (f *FlowInterlock) All() []FlowState {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]FlowState, 0, len(f.order))
	for _, lineID := range f.order {
		result = append(result, f.snapshotLocked(lineID, f.lines[lineID]))
	}
	return result
}
