package discharge

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

func testConfig() FlowConfig {
	return FlowConfig{MaxFlow: 100, ContactTimeout: 30 * time.Second, TelemetryTTL: 10 * time.Second}
}

func newHarness(t *testing.T) (*ValveController, *PermitBook, model.LineID, uuid.UUID, time.Time) {
	t.Helper()
	line := model.LineID("line-1")
	valves, err := NewValveController([]model.LineID{line, "line-2"})
	if err != nil {
		t.Fatal(err)
	}
	permits, err := NewPermitBook(valves)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	batchID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	issuePermit(t, permits, line, batchID, 1, true, now)
	return valves, permits, line, batchID, now
}

func issuePermit(t *testing.T, book *PermitBook, line model.LineID, batchID uuid.UUID, revision uint64, qualified bool, now time.Time) Permit {
	t.Helper()
	result := model.LabResult{
		SampleID:   uuid.New(),
		LineID:     line,
		BatchID:    batchID,
		Revision:   revision,
		COD:        20,
		Ammonia:    1,
		Valid:      true,
		ReceivedAt: now,
	}
	if !qualified {
		result.COD = 999
	}
	permit, err := book.ApplyResult(result, now)
	if qualified && err != nil {
		t.Fatalf("issue permit: %v", err)
	}
	return permit
}

func enableInterlock(t *testing.T, permits *PermitBook, valves *ValveController, now time.Time) (*FlowInterlock, *tripLog) {
	t.Helper()
	log := &tripLog{}
	interlock, err := NewFlowInterlock(permits, valves, testConfig(), log.record)
	if err != nil {
		t.Fatal(err)
	}
	if err := interlock.EnableLine("line-1", now); err != nil {
		t.Fatal(err)
	}
	if err := interlock.EnableLine("line-2", now); err != nil {
		t.Fatal(err)
	}
	return interlock, log
}

type tripLog struct {
	records []TripRecord
}

func (l *tripLog) record(record TripRecord) {
	l.records = append(l.records, record)
}

func reading(line model.LineID, value float64, observed time.Time) FlowReading {
	return FlowReading{LineID: line, Value: value, ObservedAt: observed}
}

func TestFlowInterlockKeepsValveOpenWithinLimit(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)

	state, err := interlock.Report(reading(line, 80, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ValveOpen || state.Tripped {
		t.Fatalf("expected open and healthy, got open=%v tripped=%v", state.ValveOpen, state.Tripped)
	}
	if len(trips.records) != 0 {
		t.Fatalf("expected no trip notifications, got %d", len(trips.records))
	}
	if !valves.IsOpen(line) {
		t.Fatal("valve should remain open for in-limit flow")
	}
}

func TestFlowInterlockTripsOnOverLimit(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)
	if _, err := interlock.Report(reading(line, 80, now), now); err != nil {
		t.Fatal(err)
	}

	state, err := interlock.Report(reading(line, 120, now.Add(time.Second)), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.ValveOpen {
		t.Fatalf("expected tripped and closed, got tripped=%v open=%v", state.Tripped, state.ValveOpen)
	}
	if state.Trip == nil || state.Trip.Reason != TripFlowOverLimit {
		t.Fatalf("expected flow_over_limit trip, got %+v", state.Trip)
	}
	if state.Trip.Flow != 120 || state.Trip.Limit != 100 {
		t.Fatalf("trip evidence mismatch: %+v", state.Trip)
	}
	if valves.IsOpen(line) {
		t.Fatal("valve must be forced shut on over-limit flow")
	}
	if len(trips.records) != 1 || trips.records[0].Reason != TripFlowOverLimit || trips.records[0].LineID != line {
		t.Fatalf("expected one scoped trip notification, got %+v", trips.records)
	}
}

func TestFlowInterlockStaleTelemetryOnArrival(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)

	stamp := now.Add(-15 * time.Second)
	state, err := interlock.Report(reading(line, 80, stamp), now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.Trip.Reason != TripTelemetryStale || state.ValveOpen {
		t.Fatalf("expected stale trip with closed valve, got %+v", state)
	}
	if len(trips.records) != 1 || trips.records[0].Reason != TripTelemetryStale {
		t.Fatalf("expected stale notification, got %+v", trips.records)
	}
}

func TestFlowInterlockRejectsStaleTelemetryWhenValveAlreadyClosed(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, _ := enableInterlock(t, permits, valves, now)
	if err := valves.Hold(line, now); err != nil {
		t.Fatal(err)
	}
	if _, err := interlock.Report(reading(line, 80, now.Add(-15*time.Second)), now); err == nil {
		t.Fatal("expected stale telemetry to be rejected when valve is closed")
	}
}

func TestFlowInterlockTripsOnLostContact(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)
	if _, err := interlock.Report(reading(line, 80, now), now); err != nil {
		t.Fatal(err)
	}

	// Reports stop; contact timeout elapses.
	states := interlock.Sweep(now.Add(31 * time.Second))
	if len(states) != 2 {
		t.Fatalf("expected states for both lines, got %d", len(states))
	}
	state := states[0]
	if state.LineID != line {
		t.Fatalf("unexpected line ordering: %s", state.LineID)
	}
	if !state.Tripped || state.Trip.Reason != TripTelemetryLost || state.ValveOpen {
		t.Fatalf("expected lost-contact trip with closed valve, got %+v", state)
	}
	if valves.IsOpen(line) {
		t.Fatal("valve must be forced shut on lost contact")
	}
	if len(trips.records) != 1 || trips.records[0].Reason != TripTelemetryLost {
		t.Fatalf("expected one lost notification, got %+v", trips.records)
	}
}

func TestFlowInterlockTripsOnStaleTelemetryDuringSweep(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, _ := enableInterlock(t, permits, valves, now)
	// TTL is 10s and contact timeout 30s: an aging reading trips stale first.
	if _, err := interlock.Report(reading(line, 80, now), now); err != nil {
		t.Fatal(err)
	}
	states := interlock.Sweep(now.Add(15 * time.Second))
	state := states[0]
	if !state.Tripped || state.Trip.Reason != TripTelemetryStale {
		t.Fatalf("expected stale trip during sweep, got %+v", state.Trip)
	}
}

func TestFlowInterlockTripIsScopedToLine(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	other := model.LineID("line-2")
	otherBatch := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	issuePermit(t, permits, other, otherBatch, 1, true, now)
	interlock, trips := enableInterlock(t, permits, valves, now)

	if _, err := interlock.Report(reading(line, 80, now), now); err != nil {
		t.Fatal(err)
	}
	if _, err := interlock.Report(reading(other, 60, now), now); err != nil {
		t.Fatal(err)
	}
	if _, err := interlock.Report(reading(line, 150, now.Add(time.Second)), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if !valves.IsOpen(other) {
		t.Fatal("tripping one line must not close another line's valve")
	}
	otherState, err := interlock.Report(reading(other, 61, now.Add(2*time.Second)), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if otherState.Tripped || !otherState.ValveOpen {
		t.Fatalf("other line should keep discharging: %+v", otherState)
	}
	if len(trips.records) != 1 || trips.records[0].LineID != line {
		t.Fatalf("expected exactly one trip on line-1, got %+v", trips.records)
	}
}

func TestFlowInterlockLatchUntilReset(t *testing.T) {
	valves, permits, line, batchID, now := newHarness(t)
	interlock, _ := enableInterlock(t, permits, valves, now)
	later := now.Add(time.Second)
	if _, err := interlock.Report(reading(line, 150, later), later); err != nil {
		t.Fatal(err)
	}

	// Fresh, good data while tripped must not auto-clear the latch.
	recovered := now.Add(2 * time.Second)
	state, err := interlock.Report(reading(line, 50, recovered), recovered)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.ValveOpen {
		t.Fatal("good telemetry must not auto-reset a latched trip")
	}

	// Reset with the permit still active succeeds but keeps the valve held.
	state, err = interlock.Reset(line, recovered)
	if err != nil {
		t.Fatal(err)
	}
	if state.Tripped || state.ValveOpen {
		t.Fatalf("reset should clear trip yet keep valve held, got %+v", state)
	}
	if state.ResetAt == nil {
		t.Fatal("reset should be timestamped")
	}

	// The next fresh, in-limit report re-verifies and resumes discharge.
	resume := now.Add(3 * time.Second)
	state, err = interlock.Report(reading(line, 50, resume), resume)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ValveOpen || state.Tripped {
		t.Fatalf("expected valve to resume open, got %+v", state)
	}

	// A later reset must re-verify the permit: revocation blocks it.
	tripAt := now.Add(4 * time.Second)
	if _, err := interlock.Report(reading(line, 150, tripAt), tripAt); err != nil {
		t.Fatal(err)
	}
	issuePermit(t, permits, line, batchID, 2, false, tripAt)
	if _, err := interlock.Reset(line, tripAt); err == nil {
		t.Fatal("reset without an active permit must fail")
	}
	if valves.IsOpen(line) {
		t.Fatal("valve stays closed after failed reset")
	}

	// Re-qualification issues a new permit; reset now re-verifies and the
	// following fresh report re-opens the valve.
	requalified := now.Add(5 * time.Second)
	issuePermit(t, permits, line, batchID, 3, true, requalified)
	if _, err := interlock.Reset(line, requalified); err != nil {
		t.Fatalf("reset after re-qualification should succeed: %v", err)
	}
	resume2 := now.Add(6 * time.Second)
	state, err = interlock.Report(reading(line, 55, resume2), resume2)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ValveOpen {
		t.Fatal("valve should re-open after permit re-verification and fresh flow")
	}
}

func TestFlowInterlockTripsWhenPermitBecomesInactive(t *testing.T) {
	valves, permits, line, batchID, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)
	if _, err := interlock.Report(reading(line, 80, now), now); err != nil {
		t.Fatal(err)
	}
	// Simulate permit invalidation that has not yet driven the valve (the
	// interlock itself must close an open, unpermitted valve).
	permit, _ := permits.ActiveForLine(line)
	issuePermit(t, permits, line, batchID, 2, false, now)
	if err := valves.Open(line, permit.ID, now); err != nil {
		t.Fatal(err)
	}

	state, err := interlock.Report(reading(line, 80, now.Add(time.Second)), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.Trip.Reason != TripPermitInactive || state.ValveOpen {
		t.Fatalf("expected permit_inactive trip, got %+v", state)
	}
	if len(trips.records) != 1 || trips.records[0].Reason != TripPermitInactive {
		t.Fatalf("expected permit notification, got %+v", trips.records)
	}
}

func TestFlowInterlockReportWithoutPermitKeepsClosedValve(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, trips := enableInterlock(t, permits, valves, now)
	// Permit revocation path already holds the valve through PermitBook.
	revoke := model.LabResult{
		SampleID: uuid.New(), LineID: line,
		BatchID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Revision: 2, COD: 999, Ammonia: 1, Valid: true, ReceivedAt: now,
	}
	if _, err := permits.ApplyResult(revoke, now); err != nil {
		t.Fatal(err)
	}
	state, err := interlock.Report(reading(line, 80, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if state.ValveOpen || state.Tripped {
		t.Fatalf("reading without permit must not open valve or latch a trip, got %+v", state)
	}
	if len(trips.records) != 0 {
		t.Fatalf("no protection event expected when valve was already closed, got %+v", trips.records)
	}
}

func TestFlowInterlockConfigure(t *testing.T) {
	valves, permits, line, _, now := newHarness(t)
	interlock, _ := enableInterlock(t, permits, valves, now)

	if err := interlock.Configure(line, FlowConfig{MaxFlow: 0, ContactTimeout: time.Second, TelemetryTTL: time.Second}, now); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
	if err := interlock.Configure(line, FlowConfig{MaxFlow: 40, ContactTimeout: 5 * time.Second, TelemetryTTL: 10 * time.Second}, now); err == nil {
		t.Fatal("contact timeout shorter than ttl should be rejected")
	}
	if err := interlock.Configure(line, FlowConfig{MaxFlow: 40, ContactTimeout: 20 * time.Second, TelemetryTTL: 5 * time.Second}, now); err != nil {
		t.Fatal(err)
	}
	state, err := interlock.Report(reading(line, 41, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.Config.MaxFlow != 40 {
		t.Fatalf("expected trip against new limit, got %+v", state)
	}
	// Configuring does not clear a latched trip.
	if err := interlock.Configure(line, testConfig(), now); err != nil {
		t.Fatal(err)
	}
	state, _ = interlock.Snapshot(line)
	if !state.Tripped {
		t.Fatal("reconfiguring must not clear a latched trip")
	}
}

func TestFlowInterlockUnknownLineAndInvalidReadings(t *testing.T) {
	valves, permits, _, _, now := newHarness(t)
	interlock, _ := enableInterlock(t, permits, valves, now)

	if err := interlock.EnableLine("", now); err == nil {
		t.Fatal("empty line should be rejected")
	}
	if _, err := interlock.Report(reading("line-x", 1, now), now); err == nil {
		t.Fatal("report for an unenabled line should fail")
	}
	if _, err := interlock.Reset("line-x", now); err == nil {
		t.Fatal("reset for an unenabled line should fail")
	}
	bad := reading("line-1", -1, now)
	if _, err := interlock.Report(bad, now); err == nil {
		t.Fatal("negative flow should be rejected")
	}
}

func TestNewFlowInterlockValidation(t *testing.T) {
	valves, err := NewValveController([]model.LineID{"line-1"})
	if err != nil {
		t.Fatal(err)
	}
	permits, err := NewPermitBook(valves)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFlowInterlock(nil, valves, testConfig(), nil); err == nil {
		t.Fatal("nil permit book should fail")
	}
	if _, err := NewFlowInterlock(permits, nil, testConfig(), nil); err == nil {
		t.Fatal("nil valve controller should fail")
	}
	bad := testConfig()
	bad.TelemetryTTL = 0
	if _, err := NewFlowInterlock(permits, valves, bad, nil); err == nil {
		t.Fatal("invalid default config should fail")
	}
}
