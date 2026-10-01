package discharge_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

type rig struct {
	clock   *testClock
	valves  *discharge.ValveController
	permits *discharge.PermitBook
	guard   *discharge.FlowInterlock
	records []discharge.TripRecord
}

func newRig(t *testing.T, lines []model.LineID, limit float64, maxAge time.Duration) *rig {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	valves, err := discharge.NewValveController(lines)
	if err != nil {
		t.Fatalf("valve controller: %v", err)
	}
	permits, err := discharge.NewPermitBook(valves)
	if err != nil {
		t.Fatalf("permit book: %v", err)
	}
	limits := make(map[model.LineID]float64, len(lines))
	for _, lineID := range lines {
		limits[lineID] = limit
	}
	r := &rig{clock: clock, valves: valves, permits: permits}
	guard, err := discharge.NewFlowInterlock(valves, permits, limits, maxAge, clock.Now, func(record discharge.TripRecord) {
		r.records = append(r.records, record)
	})
	if err != nil {
		t.Fatalf("flow interlock: %v", err)
	}
	r.guard = guard
	return r
}

func (r *rig) issuePermit(t *testing.T, lineID model.LineID) discharge.Permit {
	t.Helper()
	result := model.LabResult{
		SampleID: uuid.New(), LineID: lineID, BatchID: uuid.New(), Revision: 1,
		COD: 10, Ammonia: 1, Valid: true, ReceivedAt: r.clock.Now(),
	}
	permit, err := r.permits.ApplyResult(result, r.clock.Now())
	if err != nil {
		t.Fatalf("issue permit for %s: %v", lineID, err)
	}
	return permit
}

func (r *rig) revokePermit(t *testing.T, permit discharge.Permit) {
	t.Helper()
	result := model.LabResult{
		SampleID: permit.SampleID, LineID: permit.LineID, BatchID: permit.BatchID, Revision: permit.SampleRevision + 1,
		COD: 500, Ammonia: 1, Valid: true, ReceivedAt: r.clock.Now(),
	}
	if _, err := r.permits.ApplyResult(result, r.clock.Now()); err != nil {
		t.Fatalf("revoke permit for %s: %v", permit.LineID, err)
	}
}

func (r *rig) valveOpen(t *testing.T, lineID model.LineID) bool {
	t.Helper()
	valve, ok := r.valves.State(lineID)
	if !ok {
		t.Fatalf("valve %s is not found", lineID)
	}
	return valve.Open
}

func (r *rig) report(t *testing.T, lineID model.LineID, flow float64, sequence uint64) discharge.LineGuardState {
	t.Helper()
	state, err := r.guard.ReportFlow(lineID, flow, sequence, r.clock.Now())
	if err != nil {
		t.Fatalf("report flow %s seq=%d: %v", lineID, sequence, err)
	}
	return state
}

func lastRecord(records []discharge.TripRecord) discharge.TripRecord {
	return records[len(records)-1]
}

func TestInLimitFlowKeepsValveOpen(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	state := r.report(t, "line-1", 80, 1)
	if state.Tripped {
		t.Fatalf("in-limit flow must not trip: %+v", state)
	}
	if !r.valveOpen(t, "line-1") {
		t.Fatal("valve must stay open for in-limit flow")
	}
	if state.LastSequence != 1 || state.LastFlowM3Hour != 80 {
		t.Fatalf("reading was not recorded: %+v", state)
	}
	if len(r.records) != 0 {
		t.Fatalf("no protection action expected, got %+v", r.records)
	}
}

func TestOverLimitTripsAndAffectsOnlyOwnLine(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1", "line-2"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.issuePermit(t, "line-2")
	r.report(t, "line-2", 50, 1)

	state := r.report(t, "line-1", 150, 1)
	if !state.Tripped || state.TripReason != discharge.TripFlowAboveLimit {
		t.Fatalf("expected flow_above_limit trip, got %+v", state)
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("tripped line valve must be closed")
	}
	if !r.valveOpen(t, "line-2") {
		t.Fatal("other line valve must not be affected")
	}
	other, err := r.guard.State("line-2")
	if err != nil || other.Tripped {
		t.Fatalf("other line must stay untripped: %+v err=%v", other, err)
	}
	if len(r.records) != 1 {
		t.Fatalf("expected exactly one trip record, got %+v", r.records)
	}
	record := r.records[0]
	if record.Action != discharge.ActionTripped || record.Reason != discharge.TripFlowAboveLimit {
		t.Fatalf("trip record missing reason: %+v", record)
	}
	if record.LineID != "line-1" || record.FlowM3Hour != 150 || record.LimitM3Hour != 100 {
		t.Fatalf("trip record missing values: %+v", record)
	}
	if !strings.Contains(record.Detail, "150") || !strings.Contains(record.Detail, "100") {
		t.Fatalf("trip detail must carry flow and limit: %q", record.Detail)
	}
}

func TestStaleSequenceTripsOpenValve(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 50, 5)

	if _, err := r.guard.ReportFlow("line-1", 60, 5, r.clock.Now()); err == nil {
		t.Fatal("duplicate sequence must be rejected")
	}
	state, err := r.guard.State("line-1")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.TripReason != discharge.TripTelemetryStale {
		t.Fatalf("expected telemetry_stale trip, got %+v", state)
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("stale telemetry must not keep the valve open")
	}
	if _, err := r.guard.ReportFlow("line-1", 60, 4, r.clock.Now()); err == nil {
		t.Fatal("regressing sequence must be rejected")
	}
}

func TestStaleObservedAtTripsOpenValve(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 50, 1)
	r.clock.advance(30 * time.Second)

	old := r.clock.Now().Add(-20 * time.Second) // older than the 10s freshness window
	if _, err := r.guard.ReportFlow("line-1", 60, 2, old); err == nil {
		t.Fatal("reading older than the freshness window must be rejected")
	}
	state, _ := r.guard.State("line-1")
	if !state.Tripped || state.TripReason != discharge.TripTelemetryStale {
		t.Fatalf("expected telemetry_stale trip, got %+v", state)
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("old telemetry must not keep the valve open")
	}
}

func TestLostTelemetryTripsAfterFreshnessWindow(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 50, 1)

	r.clock.advance(10 * time.Second) // exactly at the window edge: still fresh
	if records := r.guard.Evaluate(r.clock.Now()); len(records) != 0 {
		t.Fatalf("must not trip at the window edge, got %+v", records)
	}
	r.clock.advance(time.Millisecond) // past the window: contact lost
	records := r.guard.Evaluate(r.clock.Now())
	if len(records) != 1 || records[0].Reason != discharge.TripTelemetryLost {
		t.Fatalf("expected telemetry_lost trip, got %+v", records)
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("lost telemetry must not keep the valve open")
	}
}

func TestFirstReadingGetsGraceWindow(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")

	r.clock.advance(5 * time.Second) // within the grace window, no reading yet
	if records := r.guard.Evaluate(r.clock.Now()); len(records) != 0 {
		t.Fatalf("first reading must get a grace window, got %+v", records)
	}
	r.clock.advance(10 * time.Second) // grace expired, still silent
	records := r.guard.Evaluate(r.clock.Now())
	if len(records) != 1 || records[0].Reason != discharge.TripTelemetryLost {
		t.Fatalf("expected telemetry_lost after grace, got %+v", records)
	}
}

func TestInvalidPermitTripsOpenValve(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	// valve opened without any permit (defensive path)
	if err := r.valves.Open("line-1", uuid.New(), r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r.report(t, "line-1", 50, 1)
	state, _ := r.guard.State("line-1")
	if !state.Tripped || state.TripReason != discharge.TripPermitInvalid {
		t.Fatalf("expected permit_invalid trip, got %+v", state)
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("valve without a valid permit must be closed")
	}
}

func TestResetRevalidatesPermitAndReopens(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	permit := r.issuePermit(t, "line-1")
	r.report(t, "line-1", 150, 1) // trips flow_above_limit
	if r.valveOpen(t, "line-1") {
		t.Fatal("expected trip to close the valve")
	}

	r.report(t, "line-1", 60, 2) // fresh in-limit telemetry while tripped
	state, err := r.guard.Reset("line-1")
	if err != nil {
		t.Fatalf("reset must succeed with valid permit and fresh telemetry: %v", err)
	}
	if state.Tripped {
		t.Fatalf("trip must be cleared: %+v", state)
	}
	if !r.valveOpen(t, "line-1") {
		t.Fatal("reset must reopen the valve when the permit is still valid")
	}
	valve, _ := r.valves.State("line-1")
	if valve.PermitID != permit.ID {
		t.Fatalf("valve must be reopened under the re-validated permit: %+v", valve)
	}
	record := lastRecord(r.records)
	if record.Action != discharge.ActionReset || !strings.Contains(record.Detail, string(discharge.TripFlowAboveLimit)) {
		t.Fatalf("reset must be audited with the previous reason: %+v", record)
	}
}

func TestResetRefusedWhenPermitRevoked(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	permit := r.issuePermit(t, "line-1")
	r.report(t, "line-1", 150, 1) // trips
	r.revokePermit(t, permit)
	r.report(t, "line-1", 60, 2)

	if _, err := r.guard.Reset("line-1"); err == nil {
		t.Fatal("reset must re-validate the permit and refuse when it is revoked")
	}
	state, _ := r.guard.State("line-1")
	if !state.Tripped {
		t.Fatal("line must stay tripped when reset is refused")
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("valve must stay closed when reset is refused")
	}
}

func TestResetRefusedWhenTelemetryStale(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 150, 1) // trips
	r.clock.advance(30 * time.Second)

	if _, err := r.guard.Reset("line-1"); err == nil {
		t.Fatal("reset must refuse when telemetry is not fresh")
	}
	if r.valveOpen(t, "line-1") {
		t.Fatal("valve must stay closed when reset is refused")
	}
}

func TestResetWithoutTripFails(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	if _, err := r.guard.Reset("line-1"); err == nil {
		t.Fatal("reset on an untripped line must fail")
	}
}

func TestSetLimitValidationAndEffect(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	for _, bad := range []float64{0, -5, math.Inf(1), math.NaN()} {
		if err := r.guard.SetLimit("line-1", bad); err == nil {
			t.Fatalf("limit %v must be rejected", bad)
		}
	}
	if err := r.guard.SetLimit("unknown", 50); err == nil {
		t.Fatal("unknown line must be rejected")
	}
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 90, 1) // under the 100 limit
	if err := r.guard.SetLimit("line-1", 50); err != nil {
		t.Fatalf("valid limit: %v", err)
	}
	records := r.guard.Evaluate(r.clock.Now()) // 90 now exceeds the lowered limit
	if len(records) != 1 || records[0].Reason != discharge.TripFlowAboveLimit {
		t.Fatalf("lowered limit must apply on evaluation, got %+v", records)
	}
	state, _ := r.guard.State("line-1")
	if state.LimitM3Hour != 50 {
		t.Fatalf("limit not updated: %+v", state)
	}
}

func TestStaleReadingWithoutOpenValveOnlyRejected(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.report(t, "line-1", 50, 1) // valve closed: accepted, no trip
	if _, err := r.guard.ReportFlow("line-1", 60, 1, r.clock.Now()); err == nil {
		t.Fatal("stale reading must be rejected")
	}
	state, _ := r.guard.State("line-1")
	if state.Tripped {
		t.Fatal("closed valve must not trip on stale telemetry")
	}
	if len(r.records) != 0 {
		t.Fatalf("no protection action expected, got %+v", r.records)
	}
}

func TestFutureReadingRejected(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	future := r.clock.Now().Add(time.Minute)
	if _, err := r.guard.ReportFlow("line-1", 50, 1, future); err == nil {
		t.Fatal("reading observed in the future must be rejected")
	}
}

func TestUnknownLineRejected(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	if _, err := r.guard.ReportFlow("ghost", 50, 1, r.clock.Now()); err == nil {
		t.Fatal("unknown line reading must be rejected")
	}
	if _, err := r.guard.Reset("ghost"); err == nil {
		t.Fatal("unknown line reset must be rejected")
	}
	if _, err := r.guard.State("ghost"); err == nil {
		t.Fatal("unknown line state must be rejected")
	}
}

func TestTrippedLineCannotReopenWithoutReset(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 150, 1) // trips

	// something reopens the valve behind the interlock's back
	if err := r.valves.Open("line-1", uuid.New(), r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r.guard.Evaluate(r.clock.Now())
	if r.valveOpen(t, "line-1") {
		t.Fatal("tripped line must be forced closed again on evaluation")
	}
}

func TestTripIsIdempotentAndKeepsFirstReason(t *testing.T) {
	r := newRig(t, []model.LineID{"line-1"}, 100, 10*time.Second)
	r.issuePermit(t, "line-1")
	r.report(t, "line-1", 150, 1) // trips flow_above_limit
	r.clock.advance(30 * time.Second)
	r.guard.Evaluate(r.clock.Now()) // would trip telemetry_lost, but already tripped
	if len(r.records) != 1 {
		t.Fatalf("trip must be recorded once, got %+v", r.records)
	}
	state, _ := r.guard.State("line-1")
	if state.TripReason != discharge.TripFlowAboveLimit {
		t.Fatalf("first trip reason must be kept, got %+v", state)
	}
}

func TestConstructorValidation(t *testing.T) {
	clock := &testClock{now: time.Now().UTC()}
	valves, _ := discharge.NewValveController([]model.LineID{"line-1"})
	permits, _ := discharge.NewPermitBook(valves)
	limits := map[model.LineID]float64{"line-1": 100}
	if _, err := discharge.NewFlowInterlock(nil, permits, limits, time.Second, clock.Now, nil); err == nil {
		t.Fatal("nil valve controller must be rejected")
	}
	if _, err := discharge.NewFlowInterlock(valves, nil, limits, time.Second, clock.Now, nil); err == nil {
		t.Fatal("nil permit book must be rejected")
	}
	if _, err := discharge.NewFlowInterlock(valves, permits, limits, 0, clock.Now, nil); err == nil {
		t.Fatal("non-positive max age must be rejected")
	}
	if _, err := discharge.NewFlowInterlock(valves, permits, map[model.LineID]float64{"line-1": -1}, time.Second, clock.Now, nil); err == nil {
		t.Fatal("negative limit must be rejected")
	}
	if _, err := discharge.NewFlowInterlock(valves, permits, map[model.LineID]float64{"ghost": 1}, time.Second, clock.Now, nil); err == nil {
		t.Fatal("line without a valve must be rejected")
	}
}
