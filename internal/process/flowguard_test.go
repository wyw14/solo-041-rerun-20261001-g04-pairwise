package process

import (
	"context"
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

func newTestPlant(t *testing.T, clock *testClock, lines []model.LineID, limit float64, maxAge time.Duration) *Plant {
	t.Helper()
	plant, err := NewPlant(t.TempDir(), lines, clock.Now, FlowInterlockConfig{
		LimitM3Hour:     limit,
		TelemetryMaxAge: maxAge,
	})
	if err != nil {
		t.Fatalf("new plant: %v", err)
	}
	return plant
}

func qualifyLine(t *testing.T, plant *Plant, lineID model.LineID, now time.Time) {
	t.Helper()
	ctx := context.Background()
	batch, err := plant.StartBatch(ctx, lineID, 50, "integration-test")
	if err != nil {
		t.Fatalf("start batch %s: %v", lineID, err)
	}
	outcome, err := plant.ApplyLabResult(ctx, model.LabResult{
		SampleID: uuid.New(), LineID: lineID, BatchID: batch.ID, Revision: 1,
		COD: 10, Ammonia: 1, Valid: true, ReceivedAt: now,
	}, now)
	if err != nil {
		t.Fatalf("apply lab result %s: %v", lineID, err)
	}
	if outcome.Permit == nil {
		t.Fatalf("qualified result must issue a permit for %s", lineID)
	}
}

func lineValveOpen(t *testing.T, plant *Plant, lineID model.LineID) bool {
	t.Helper()
	status, err := plant.Status(lineID)
	if err != nil {
		t.Fatalf("status %s: %v", lineID, err)
	}
	for _, valve := range status.Valves {
		if valve.LineID == lineID {
			return valve.Open
		}
	}
	t.Fatalf("valve for %s not found in status", lineID)
	return false
}

func TestPlantFlowInterlockEndToEnd(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	plant := newTestPlant(t, clock, []model.LineID{"line-1", "line-2"}, 100, 5*time.Second)
	qualifyLine(t, plant, "line-1", clock.Now())
	qualifyLine(t, plant, "line-2", clock.Now())
	if !lineValveOpen(t, plant, "line-1") || !lineValveOpen(t, plant, "line-2") {
		t.Fatal("qualified lines must have open valves")
	}

	// line-1 exceeds its limit: only line-1 trips.
	if _, err := plant.ReportDischargeFlow("line-1", 140, 1, clock.Now()); err != nil {
		t.Fatalf("report flow: %v", err)
	}
	if _, err := plant.ReportDischargeFlow("line-2", 40, 1, clock.Now()); err != nil {
		t.Fatalf("report flow: %v", err)
	}
	guard1, err := plant.FlowInterlock("line-1")
	if err != nil {
		t.Fatal(err)
	}
	if !guard1.Tripped || guard1.TripReason != discharge.TripFlowAboveLimit {
		t.Fatalf("line-1 must trip flow_above_limit: %+v", guard1)
	}
	guard2, err := plant.FlowInterlock("line-2")
	if err != nil {
		t.Fatal(err)
	}
	if guard2.Tripped {
		t.Fatalf("line-2 must not be affected: %+v", guard2)
	}
	if lineValveOpen(t, plant, "line-1") {
		t.Fatal("line-1 valve must be closed by the interlock")
	}
	if !lineValveOpen(t, plant, "line-2") {
		t.Fatal("line-2 valve must stay open")
	}

	// the protection action raised an alarm on line-1 only.
	alarms := plant.Alarms()
	if len(alarms) != 1 {
		t.Fatalf("expected one interlock alarm, got %+v", alarms)
	}
	if alarms[0].Code != "DISCHARGE_FLOW_INTERLOCK" || alarms[0].LineID != "line-1" || alarms[0].Severity != model.SeverityCritical {
		t.Fatalf("unexpected interlock alarm: %+v", alarms[0])
	}

	// audit trail carries the reason.
	trips := plant.FlowInterlockTrips()
	if len(trips) != 1 || trips[0].Reason != discharge.TripFlowAboveLimit || trips[0].LineID != "line-1" {
		t.Fatalf("trip audit missing reason: %+v", trips)
	}

	// reset re-validates the permit and reopens only line-1.
	if _, err := plant.ReportDischargeFlow("line-1", 60, 2, clock.Now()); err != nil {
		t.Fatalf("report fresh flow: %v", err)
	}
	state, err := plant.ResetDischargeFlowInterlock("line-1")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if state.Tripped {
		t.Fatalf("trip must be cleared: %+v", state)
	}
	if !lineValveOpen(t, plant, "line-1") {
		t.Fatal("line-1 valve must reopen after reset")
	}

	// line status exposes the interlock state.
	status, err := plant.Status("line-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.FlowInterlock == nil || status.FlowInterlock.LineID != "line-1" || status.FlowInterlock.LimitM3Hour != 100 {
		t.Fatalf("status must carry the flow interlock: %+v", status.FlowInterlock)
	}

	if _, err := plant.ReportDischargeFlow("ghost", 1, 1, clock.Now()); err == nil {
		t.Fatal("unknown line must be rejected")
	}
}

func TestPlantFlowInterlockTelemetryLost(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	plant := newTestPlant(t, clock, []model.LineID{"line-1"}, 100, 5*time.Second)
	qualifyLine(t, plant, "line-1", clock.Now())
	if _, err := plant.ReportDischargeFlow("line-1", 60, 1, clock.Now()); err != nil {
		t.Fatalf("report flow: %v", err)
	}

	clock.advance(6 * time.Second) // no further telemetry: contact lost
	plant.EvaluateFlowInterlocks()

	guard, err := plant.FlowInterlock("line-1")
	if err != nil {
		t.Fatal(err)
	}
	if !guard.Tripped || guard.TripReason != discharge.TripTelemetryLost {
		t.Fatalf("expected telemetry_lost trip, got %+v", guard)
	}
	if lineValveOpen(t, plant, "line-1") {
		t.Fatal("silent line must not keep discharging")
	}
}
