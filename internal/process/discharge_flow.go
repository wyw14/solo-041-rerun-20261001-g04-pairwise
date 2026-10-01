package process

import (
	"errors"
	"fmt"
	"time"

	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

// ReportDischargeFlow accepts an actual discharge flow reading from one
// process line and lets the flow interlock decide whether its valve may stay
// open. Over-limit, stale telemetry or a lost permit force the valve shut and
// latch a reason record; the effect is confined to the reporting line.
func (p *Plant) ReportDischargeFlow(lineID model.LineID, value float64, observedAt time.Time) (discharge.FlowState, error) {
	if _, ok := p.lineKnown(lineID); !ok {
		return discharge.FlowState{}, errors.New("process line is not found")
	}
	if observedAt.Equal(time.Time{}) {
		observedAt = p.now()
	}
	reading := discharge.FlowReading{LineID: lineID, Value: value, ObservedAt: observedAt.UTC()}
	return p.flowGuard.Report(reading, p.now())
}

// ResetDischargeFlow clears a latched trip for one line after re-verifying
// its permit. The valve stays held until the next fresh, in-limit flow report.
func (p *Plant) ResetDischargeFlow(lineID model.LineID) (discharge.FlowState, error) {
	if _, ok := p.lineKnown(lineID); !ok {
		return discharge.FlowState{}, errors.New("process line is not found")
	}
	return p.flowGuard.Reset(lineID, p.now())
}

// ConfigureDischargeFlow updates the protection envelope of one line at
// runtime. It never clears a latched trip.
func (p *Plant) ConfigureDischargeFlow(lineID model.LineID, config discharge.FlowConfig) (discharge.FlowState, error) {
	if _, ok := p.lineKnown(lineID); !ok {
		return discharge.FlowState{}, errors.New("process line is not found")
	}
	if err := p.flowGuard.Configure(lineID, config, p.now()); err != nil {
		return discharge.FlowState{}, err
	}
	return p.flowGuard.Snapshot(lineID)
}

// DischargeFlowStatus returns the interlock state of one line.
func (p *Plant) DischargeFlowStatus(lineID model.LineID) (discharge.FlowState, error) {
	if _, ok := p.lineKnown(lineID); !ok {
		return discharge.FlowState{}, errors.New("process line is not found")
	}
	return p.flowGuard.Snapshot(lineID)
}

// DischargeFlowStatusAll returns the interlock state of every supervised line.
func (p *Plant) DischargeFlowStatusAll() []discharge.FlowState {
	return p.flowGuard.All()
}

// SweepDischargeFlow enforces contact-timeout protection on every line. It is
// driven by the maintenance ticker so a line that stops reporting cannot keep
// its valve open.
func (p *Plant) SweepDischargeFlow() []discharge.FlowState {
	return p.flowGuard.Sweep(p.now())
}

func (p *Plant) lineKnown(lineID model.LineID) (model.LineState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state, ok := p.lines[lineID]
	return state, ok
}

func tripAlarmCode(reason discharge.TripReason) string {
	switch reason {
	case discharge.TripFlowOverLimit:
		return "DISCHARGE_FLOW_OVER_LIMIT"
	case discharge.TripTelemetryStale:
		return "DISCHARGE_FLOW_STALE"
	case discharge.TripTelemetryLost:
		return "DISCHARGE_FLOW_LOST"
	case discharge.TripPermitInactive:
		return "DISCHARGE_PERMIT_INVALID"
	default:
		return "DISCHARGE_FLOW_TRIP"
	}
}

func tripAlarmMessage(record discharge.TripRecord) string {
	return fmt.Sprintf("discharge valve held on line %s: %s", record.LineID, record.Message)
}
