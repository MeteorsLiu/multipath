// Package rtt provides an RFC 6298 SRTT/RTTVAR estimator used by transport
// observers.
package rtt

// Estimator tracks smoothed RTT using the RFC 6298 SRTT/RTTVAR update rule.
//
// Estimator is not internally synchronized. Callers that share one estimator
// across goroutines must provide their own locking.
type Estimator struct {
	srttMS   uint32
	rttvarMS uint32
	samples  uint32
}

// Add incorporates one RTT sample in milliseconds.
func (e *Estimator) Add(sampleMS uint32) {
	if e.samples == 0 {
		e.srttMS = sampleMS
		e.rttvarMS = sampleMS / 2
		e.samples = 1
		return
	}

	delta := absDiff(e.srttMS, sampleMS)
	e.rttvarMS = (3*e.rttvarMS + delta) / 4
	e.srttMS = (7*e.srttMS + sampleMS) / 8
	e.samples++
}

// SRTT returns the current smoothed RTT and whether at least one sample exists.
func (e Estimator) SRTT() (uint32, bool) {
	if e.samples == 0 {
		return 0, false
	}
	return e.srttMS, true
}

// RTTVAR returns the current RTT variation estimate and whether at least one
// sample exists.
func (e Estimator) RTTVAR() (uint32, bool) {
	if e.samples == 0 {
		return 0, false
	}
	return e.rttvarMS, true
}

// Samples returns the number of samples incorporated by the estimator.
func (e Estimator) Samples() uint32 {
	return e.samples
}

func absDiff(a, b uint32) uint32 {
	if a >= b {
		return a - b
	}
	return b - a
}
