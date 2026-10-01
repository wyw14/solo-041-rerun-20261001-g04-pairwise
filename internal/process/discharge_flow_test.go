package process

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

func newTestPlant(t *testing.T) (*Plant, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	config := discharge.FlowConfig{MaxFlow: 100, ContactTimeout: 30 * time.Second, TelemetryTTL: 10 * time.Second}
	plant, err := NewPlant(t.TempDir(), []model.LineID{"line-1", "line-2"}, clock, config)
	if err != nil {
		t.Fatal(err)
	}
	return plant, &now
}

func advanceToSettling(t *testing.T, plant *Plant, ctx context.Context, lineID model.LineID) {
	t.Helper()
	for _, stage := range []model.ProcessStage{
		model.StageEqualizing, model.StageBiological, model.StageSettling,
	} {
		if _, err := plant.AdvanceBatch(ctx, lineID, stage); err != nil {
			t.Fatalf("advance to %s: %v", stage, err)
		}
	}
}

func qualifyBatch(t *testing.T, plant *Plant, ctx context.Context, lineID model.LineID, batchID string, revision uint64, cod float64) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sample_id": "33333333-3333-3333-3333-" + padRevision(revision),
		"line_id":   lineID,
		"batch_id":  batchID,
		"revision":  revision,
		"cod":       cod,
		"ammonia":   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plant.SubmitLabPayload(ctx, payload); err != nil {
		t.Fatalf("submit lab payload: %v", err)
	}
}

func padRevision(revision uint64) string {
	if revision == 1 {
		return "333333333331"
	}
	return "333333333332"
}

func TestDischargeFlowInterlockEndToEnd(t *testing.T) {
	ctx := context.Background()
	plant, now := newTestPlant(t)
	line := model.LineID("line-1")

	batch, err := plant.StartBatch(ctx, line, 120, "influent-a")
	if err != nil {
		t.Fatal(err)
	}
	advanceToSettling(t, plant, ctx, line)
	qualifyBatch(t, plant, ctx, line, batch.ID.String(), 1, 20)

	if !plant.valves.IsOpen(line) {
		t.Fatal("qualified result should open the discharge valve")
	}

	// In-limit actual flow keeps the valve open.
	state, err := plant.ReportDischargeFlow(line, 80, *now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ValveOpen || state.Tripped {
		t.Fatalf("expected healthy discharge, got %+v", state)
	}

	// Over-limit flow forces the valve shut, records the reason and raises a
	// line-scoped critical alarm.
	*now = now.Add(2 * time.Second)
	state, err = plant.ReportDischargeFlow(line, 130, *now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.ValveOpen || state.Trip.Reason != discharge.TripFlowOverLimit {
		t.Fatalf("expected over-limit trip, got %+v", state)
	}
	if plant.valves.IsOpen(line) {
		t.Fatal("valve must be held after an over-limit trip")
	}
	active := plant.alarms.Active(line)
	if len(active) != 1 || active[0].Severity != model.SeverityCritical ||
		active[0].Code != "DISCHARGE_FLOW_OVER_LIMIT" {
		t.Fatalf("expected one critical discharge alarm on the line, got %+v", active)
	}

	// The other line is untouched.
	for _, other := range plant.DischargeFlowStatusAll() {
		if other.LineID == "line-2" && (other.Tripped || other.ValveOpen) {
			t.Fatalf("line-2 must be unaffected: %+v", other)
		}
	}

	// Fresh good data must not auto-clear the latch.
	*now = now.Add(time.Second)
	if state, err = plant.ReportDischargeFlow(line, 50, *now); err != nil {
		t.Fatal(err)
	}
	if !state.Tripped || state.ValveOpen {
		t.Fatal("latch must hold until an explicit reset")
	}

	// Reset re-verifies the permit, then the next fresh in-limit report
	// re-opens the valve.
	if state, err = plant.ResetDischargeFlow(line); err != nil {
		t.Fatalf("reset with active permit: %v", err)
	}
	if state.Tripped || state.ValveOpen {
		t.Fatalf("reset clears trip but keeps valve held: %+v", state)
	}
	*now = now.Add(time.Second)
	if state, err = plant.ReportDischargeFlow(line, 50, *now); err != nil {
		t.Fatal(err)
	}
	if !state.ValveOpen || state.Tripped {
		t.Fatalf("discharge should resume after reset and fresh flow: %+v", state)
	}

	// The per-line status payload exposes the interlock.
	status, err := plant.Status(line)
	if err != nil {
		t.Fatal(err)
	}
	if status.FlowGuard == nil || status.FlowGuard.LineID != line || !status.FlowGuard.ValveOpen {
		t.Fatalf("line status should expose flow guard state, got %+v", status.FlowGuard)
	}
}

func TestDischargeFlowResetRequiresValidPermit(t *testing.T) {
	ctx := context.Background()
	plant, now := newTestPlant(t)
	line := model.LineID("line-1")

	batch, err := plant.StartBatch(ctx, line, 120, "influent-a")
	if err != nil {
		t.Fatal(err)
	}
	advanceToSettling(t, plant, ctx, line)
	qualifyBatch(t, plant, ctx, line, batch.ID.String(), 1, 20)

	*now = now.Add(time.Second)
	if _, err := plant.ReportDischargeFlow(line, 130, *now); err != nil {
		t.Fatal(err)
	}
	// A disqualifying revision revokes the permit (and holds the valve).
	*now = now.Add(time.Second)
	qualifyBatch(t, plant, ctx, line, batch.ID.String(), 2, 999)
	if _, err := plant.ResetDischargeFlow(line); err == nil {
		t.Fatal("reset must fail while the permit is invalid")
	}
}

func TestDischargeFlowLostContactTrips(t *testing.T) {
	ctx := context.Background()
	plant, now := newTestPlant(t)
	line := model.LineID("line-1")

	batch, err := plant.StartBatch(ctx, line, 120, "influent-a")
	if err != nil {
		t.Fatal(err)
	}
	advanceToSettling(t, plant, ctx, line)
	qualifyBatch(t, plant, ctx, line, batch.ID.String(), 1, 20)
	if _, err := plant.ReportDischargeFlow(line, 80, *now); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(31 * time.Second)
	states := plant.SweepDischargeFlow()
	var state discharge.FlowState
	for _, candidate := range states {
		if candidate.LineID == line {
			state = candidate
		}
	}
	if !state.Tripped || state.Trip.Reason != discharge.TripTelemetryLost || state.ValveOpen {
		t.Fatalf("expected lost-contact trip, got %+v", state)
	}
}
