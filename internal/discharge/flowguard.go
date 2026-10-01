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

// TripReason identifies why the flow interlock forced a discharge valve closed.
type TripReason string

const (
	TripFlowAboveLimit TripReason = "flow_above_limit" // 实际流量超过配置上限
	TripTelemetryLost  TripReason = "telemetry_lost"   // 遥测失联：新鲜窗口内无有效上报
	TripTelemetryStale TripReason = "telemetry_stale"  // 旧遥测：序列不推进或观测时间过旧
	TripPermitInvalid  TripReason = "permit_invalid"   // 许可失效（兜底）
)

// Protection record actions recorded in the audit trail.
const (
	ActionTripped = "tripped"
	ActionReset   = "reset"
)

const maxTripRecords = 256

// FlowReading is one actual-flow telemetry report from a process line.
type FlowReading struct {
	LineID     model.LineID `json:"line_id"`
	FlowM3Hour float64      `json:"flow_m3_hour"`
	Sequence   uint64       `json:"sequence"`
	ObservedAt time.Time    `json:"observed_at"`
	ReceivedAt time.Time    `json:"received_at"`
}

// TripRecord is the audit entry written for every protection action and reset.
type TripRecord struct {
	ID          uuid.UUID    `json:"id"`
	LineID      model.LineID `json:"line_id"`
	Action      string       `json:"action"`
	Reason      TripReason   `json:"reason,omitempty"`
	Detail      string       `json:"detail"`
	FlowM3Hour  float64      `json:"flow_m3_hour"`
	LimitM3Hour float64      `json:"limit_m3_hour"`
	At          time.Time    `json:"at"`
}

// LineGuardState is the per-line interlock snapshot exposed over the API.
type LineGuardState struct {
	LineID         model.LineID `json:"line_id"`
	LimitM3Hour    float64      `json:"limit_m3_hour"`
	Tripped        bool         `json:"tripped"`
	TripReason     TripReason   `json:"trip_reason,omitempty"`
	TripDetail     string       `json:"trip_detail,omitempty"`
	LastSequence   uint64       `json:"last_sequence"`
	LastFlowM3Hour float64      `json:"last_flow_m3_hour"`
	LastObservedAt *time.Time   `json:"last_observed_at,omitempty"`
	LastReceivedAt *time.Time   `json:"last_received_at,omitempty"`
	TrippedAt      *time.Time   `json:"tripped_at,omitempty"`
}

type lineGuard struct {
	last      FlowReading
	hasLast   bool
	tripped   bool
	reason    TripReason
	detail    string
	trippedAt time.Time
}

// FlowInterlock supervises discharge valves: a valve may stay open only while
// the line holds an active permit and fresh, in-limit flow telemetry.
// All state is keyed by line, so a trip or reset never touches other lines.
type FlowInterlock struct {
	mu      sync.Mutex
	valves  *ValveController
	permits *PermitBook
	maxAge  time.Duration
	limits  map[model.LineID]float64
	lines   map[model.LineID]*lineGuard
	trips   []TripRecord
	now     func() time.Time
	onTrip  func(TripRecord)
}

func NewFlowInterlock(valves *ValveController, permits *PermitBook, limits map[model.LineID]float64, maxAge time.Duration, now func() time.Time, onTrip func(TripRecord)) (*FlowInterlock, error) {
	if valves == nil || permits == nil || now == nil {
		return nil, errors.New("flow interlock requires valve controller, permit book and clock")
	}
	if maxAge <= 0 {
		return nil, errors.New("flow telemetry max age must be positive")
	}
	if len(limits) == 0 {
		return nil, errors.New("flow interlock limits are required")
	}
	guard := &FlowInterlock{
		valves: valves, permits: permits, maxAge: maxAge,
		limits: make(map[model.LineID]float64, len(limits)),
		lines:  make(map[model.LineID]*lineGuard, len(limits)),
		now:    now, onTrip: onTrip,
	}
	for lineID, limit := range limits {
		if lineID == "" {
			return nil, errors.New("flow interlock line is empty")
		}
		if err := validateLimit(limit); err != nil {
			return nil, err
		}
		if _, ok := valves.State(lineID); !ok {
			return nil, fmt.Errorf("flow interlock line %s has no discharge valve", lineID)
		}
		guard.limits[lineID] = limit
		guard.lines[lineID] = &lineGuard{}
	}
	return guard, nil
}

func validateLimit(limit float64) error {
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
		return errors.New("flow limit must be a positive finite value")
	}
	return nil
}

// ReportFlow ingests one actual-flow reading for a line and immediately
// evaluates whether that line's valve may remain open. Stale readings are
// rejected and, while the valve is open, force a protective trip.
func (g *FlowInterlock) ReportFlow(lineID model.LineID, flow float64, sequence uint64, observedAt time.Time) (LineGuardState, error) {
	if sequence == 0 || observedAt.IsZero() || math.IsNaN(flow) || math.IsInf(flow, 0) || flow < 0 {
		return LineGuardState{}, errors.New("flow reading is invalid")
	}
	now := g.now().UTC()
	observedAt = observedAt.UTC()
	if observedAt.After(now) {
		return LineGuardState{}, errors.New("flow reading is observed in the future")
	}
	g.mu.Lock()
	line, ok := g.lines[lineID]
	if !ok {
		g.mu.Unlock()
		return LineGuardState{}, errors.New("flow interlock line is not found")
	}
	g.reassertLocked(lineID, line, now)
	var record *TripRecord
	var readErr error
	switch {
	case line.hasLast && (sequence <= line.last.Sequence || observedAt.Before(line.last.ObservedAt)):
		readErr = errors.New("flow reading is stale")
		if !line.tripped && g.valveOpenLocked(lineID) {
			record = g.tripLocked(lineID, line, TripTelemetryStale,
				fmt.Sprintf("stale flow telemetry rejected: sequence=%d last_sequence=%d", sequence, line.last.Sequence), now)
		}
	case now.Sub(observedAt) > g.maxAge:
		readErr = errors.New("flow reading is older than the freshness window")
		if !line.tripped && g.valveOpenLocked(lineID) {
			record = g.tripLocked(lineID, line, TripTelemetryStale,
				fmt.Sprintf("flow reading observed %s ago exceeds freshness window %s", now.Sub(observedAt).Round(time.Millisecond), g.maxAge), now)
		}
	default:
		line.last = FlowReading{LineID: lineID, FlowM3Hour: flow, Sequence: sequence, ObservedAt: observedAt, ReceivedAt: now}
		line.hasLast = true
		if !line.tripped && g.valveOpenLocked(lineID) {
			record = g.evaluateLocked(lineID, line, now)
		}
	}
	state := g.stateLocked(lineID, line)
	g.mu.Unlock()
	if record != nil {
		g.notify(*record)
	}
	if readErr != nil {
		return state, readErr
	}
	return state, nil
}

// Evaluate re-checks every line; it is the periodic fail-safe that closes
// valves whose telemetry went silent or whose permit disappeared.
func (g *FlowInterlock) Evaluate(now time.Time) []TripRecord {
	now = now.UTC()
	g.mu.Lock()
	var records []TripRecord
	for lineID, line := range g.lines {
		if line.tripped {
			g.reassertLocked(lineID, line, now)
			continue
		}
		if !g.valveOpenLocked(lineID) {
			continue
		}
		if record := g.evaluateLocked(lineID, line, now); record != nil {
			records = append(records, *record)
		}
	}
	g.mu.Unlock()
	for _, record := range records {
		g.notify(record)
	}
	return records
}

// Reset clears a trip on one line after re-validating the discharge permit
// and telemetry freshness, then reopens that line's valve. Other lines are
// not affected.
func (g *FlowInterlock) Reset(lineID model.LineID) (LineGuardState, error) {
	now := g.now().UTC()
	g.mu.Lock()
	line, ok := g.lines[lineID]
	if !ok {
		g.mu.Unlock()
		return LineGuardState{}, errors.New("flow interlock line is not found")
	}
	if !line.tripped {
		state := g.stateLocked(lineID, line)
		g.mu.Unlock()
		return state, errors.New("flow interlock is not tripped")
	}
	permit, ok := g.permits.ActiveForLine(lineID)
	if !ok {
		state := g.stateLocked(lineID, line)
		g.mu.Unlock()
		return state, errors.New("discharge permit is not active; reset refused")
	}
	if !line.hasLast || now.Sub(line.last.ReceivedAt) > g.maxAge {
		state := g.stateLocked(lineID, line)
		g.mu.Unlock()
		return state, errors.New("flow telemetry is not fresh; reset refused")
	}
	if line.last.FlowM3Hour > g.limits[lineID] {
		state := g.stateLocked(lineID, line)
		g.mu.Unlock()
		return state, errors.New("flow is above the limit; reset refused")
	}
	previousReason := line.reason
	line.tripped = false
	line.reason = ""
	line.detail = ""
	line.trippedAt = time.Time{}
	if err := g.valves.Open(lineID, permit.ID, now); err != nil {
		line.tripped = true
		line.reason = previousReason
		g.mu.Unlock()
		return LineGuardState{}, fmt.Errorf("reopen discharge valve: %w", err)
	}
	record := TripRecord{
		ID: uuid.New(), LineID: lineID, Action: ActionReset,
		Detail:      fmt.Sprintf("interlock reset after %s; permit %s re-validated", previousReason, permit.ID),
		FlowM3Hour:  line.last.FlowM3Hour,
		LimitM3Hour: g.limits[lineID],
		At:          now,
	}
	g.appendTripLocked(record)
	state := g.stateLocked(lineID, line)
	g.mu.Unlock()
	g.notify(record)
	return state, nil
}

// SetLimit reconfigures the flow limit of one line; the new limit applies on
// the next reading or evaluation cycle.
func (g *FlowInterlock) SetLimit(lineID model.LineID, limit float64) error {
	if err := validateLimit(limit); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.lines[lineID]; !ok {
		return errors.New("flow interlock line is not found")
	}
	g.limits[lineID] = limit
	return nil
}

func (g *FlowInterlock) State(lineID model.LineID) (LineGuardState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	line, ok := g.lines[lineID]
	if !ok {
		return LineGuardState{}, errors.New("flow interlock line is not found")
	}
	return g.stateLocked(lineID, line), nil
}

func (g *FlowInterlock) Snapshot() []LineGuardState {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make([]LineGuardState, 0, len(g.lines))
	for lineID, line := range g.lines {
		result = append(result, g.stateLocked(lineID, line))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LineID < result[j].LineID })
	return result
}

// Trips returns the bounded audit trail of protection actions, newest first.
func (g *FlowInterlock) Trips() []TripRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make([]TripRecord, len(g.trips))
	copy(result, g.trips)
	sort.Slice(result, func(i, j int) bool { return result[i].At.After(result[j].At) })
	return result
}

// evaluateLocked inspects one open, untripped line and trips it when the
// permit is gone or telemetry is missing, silent or above the limit.
func (g *FlowInterlock) evaluateLocked(lineID model.LineID, line *lineGuard, now time.Time) *TripRecord {
	if _, ok := g.permits.ActiveForLine(lineID); !ok {
		return g.tripLocked(lineID, line, TripPermitInvalid, "discharge permit is not active", now)
	}
	if !line.hasLast {
		// The first reading gets one freshness window of grace, counted
		// from when the valve was actually opened.
		if valve, ok := g.valves.State(lineID); ok && valve.Open && now.Sub(valve.UpdatedAt) <= g.maxAge {
			return nil
		}
		return g.tripLocked(lineID, line, TripTelemetryLost, "no flow telemetry received", now)
	}
	if silent := now.Sub(line.last.ReceivedAt); silent > g.maxAge {
		return g.tripLocked(lineID, line, TripTelemetryLost,
			fmt.Sprintf("no flow telemetry for %s exceeds freshness window %s", silent.Round(time.Millisecond), g.maxAge), now)
	}
	if limit := g.limits[lineID]; line.last.FlowM3Hour > limit {
		return g.tripLocked(lineID, line, TripFlowAboveLimit,
			fmt.Sprintf("flow %.2f m3/h exceeds limit %.2f m3/h", line.last.FlowM3Hour, limit), now)
	}
	return nil
}

// tripLocked closes the line's valve and records the protection action. It is
// idempotent: an already tripped line keeps its first trip reason.
func (g *FlowInterlock) tripLocked(lineID model.LineID, line *lineGuard, reason TripReason, detail string, now time.Time) *TripRecord {
	if line.tripped {
		return nil
	}
	line.tripped = true
	line.reason = reason
	line.detail = detail
	line.trippedAt = now
	_ = g.valves.Hold(lineID, now)
	record := TripRecord{
		ID: uuid.New(), LineID: lineID, Action: ActionTripped, Reason: reason, Detail: detail,
		FlowM3Hour: line.last.FlowM3Hour, LimitM3Hour: g.limits[lineID], At: now,
	}
	g.appendTripLocked(record)
	return &record
}

// reassertLocked forces the valve closed when a tripped line is somehow open,
// e.g. a permit reopened it while the trip is still active.
func (g *FlowInterlock) reassertLocked(lineID model.LineID, line *lineGuard, now time.Time) {
	if line.tripped && g.valveOpenLocked(lineID) {
		_ = g.valves.Hold(lineID, now)
	}
}

func (g *FlowInterlock) valveOpenLocked(lineID model.LineID) bool {
	valve, ok := g.valves.State(lineID)
	return ok && valve.Open
}

func (g *FlowInterlock) appendTripLocked(record TripRecord) {
	g.trips = append(g.trips, record)
	if len(g.trips) > maxTripRecords {
		g.trips = append([]TripRecord(nil), g.trips[len(g.trips)-maxTripRecords:]...)
	}
}

func (g *FlowInterlock) stateLocked(lineID model.LineID, line *lineGuard) LineGuardState {
	state := LineGuardState{
		LineID: lineID, LimitM3Hour: g.limits[lineID], Tripped: line.tripped,
		TripReason: line.reason, TripDetail: line.detail, LastSequence: line.last.Sequence,
	}
	if line.hasLast {
		state.LastFlowM3Hour = line.last.FlowM3Hour
		observed := line.last.ObservedAt
		received := line.last.ReceivedAt
		state.LastObservedAt = &observed
		state.LastReceivedAt = &received
	}
	if line.tripped && !line.trippedAt.IsZero() {
		stamp := line.trippedAt
		state.TrippedAt = &stamp
	}
	return state
}

func (g *FlowInterlock) notify(record TripRecord) {
	if g.onTrip != nil {
		g.onTrip(record)
	}
}
