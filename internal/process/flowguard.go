package process

import (
	"errors"
	"time"

	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

// FlowInterlockConfig tunes the discharge flow interlock; every line starts
// with the same limit, which can be reconfigured per line at runtime.
type FlowInterlockConfig struct {
	LimitM3Hour     float64
	TelemetryMaxAge time.Duration
}

// ReportDischargeFlow ingests one actual-flow telemetry reading for a line.
// The interlock may force the line's valve closed as a result.
func (p *Plant) ReportDischargeFlow(lineID model.LineID, flow float64, sequence uint64, observedAt time.Time) (discharge.LineGuardState, error) {
	p.mu.RLock()
	_, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok {
		return discharge.LineGuardState{}, errors.New("process line is not found")
	}
	return p.flowGuard.ReportFlow(lineID, flow, sequence, observedAt)
}

// EvaluateFlowInterlocks re-checks every supervised line; called from the
// maintenance loop so silent lines cannot keep discharging.
func (p *Plant) EvaluateFlowInterlocks() {
	p.flowGuard.Evaluate(p.now())
}

// ResetDischargeFlowInterlock clears one line's trip after re-validating its
// permit and telemetry, then reopens that line's valve.
func (p *Plant) ResetDischargeFlowInterlock(lineID model.LineID) (discharge.LineGuardState, error) {
	return p.flowGuard.Reset(lineID)
}

// SetDischargeFlowLimit reconfigures one line's flow limit.
func (p *Plant) SetDischargeFlowLimit(lineID model.LineID, limit float64) error {
	return p.flowGuard.SetLimit(lineID, limit)
}

// FlowInterlocks returns the interlock state of every supervised line.
func (p *Plant) FlowInterlocks() []discharge.LineGuardState {
	return p.flowGuard.Snapshot()
}

// FlowInterlock returns the interlock state of one line.
func (p *Plant) FlowInterlock(lineID model.LineID) (discharge.LineGuardState, error) {
	return p.flowGuard.State(lineID)
}

// FlowInterlockTrips returns the protection action audit trail, newest first.
func (p *Plant) FlowInterlockTrips() []discharge.TripRecord {
	return p.flowGuard.Trips()
}
