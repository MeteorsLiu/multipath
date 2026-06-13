package send

import (
	"context"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

// dialBackoffSteps is the TCP redial backoff schedule (spec 5.6, 7.4): 5→10→20→30s,
// then it holds at the last value.
var dialBackoffSteps = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
}

// dialFunc dials the stream transport to remote, returning the bound TCP ref.
// It is the injected StreamTransport.Dial (spec 5.6) so the dialer never imports
// a concrete transport implementation.
type dialFunc func(ctx context.Context, remote string) (transport.LegRef, error)

// dialer drives TCP (re)dial for one lane, off the data path (spec 5.6). UDP is
// config-injected and never dialed; only TCP goes through the dialer. The dialer
// is non-blocking: lane creation kicks off start() in a goroutine and Write keeps
// using UDP until TCP is bound and active.
//
// On a successful dial it calls onDialed(ref) (which binds the ref into the leg
// and kicks off the TCP HELLO handshake). On failure it backs off and retries.
// A redial is requested by calling redial() after the leg marks TCP down
// (OnLegFailure / HELLO expire).
type dialer struct {
	remote   string
	dial     dialFunc
	onDialed func(ref transport.LegRef)

	// wake is signaled to (re)start a dial attempt promptly. Buffered depth 1 so a
	// redial request while idle is not lost and a duplicate request coalesces.
	wake chan struct{}
}

// newDialer builds a dialer for remote. dial and onDialed are injected by send
// when it creates the lane. A nil dial (no stream transport configured) yields a
// dialer whose start() is a no-op, so UDP-only lanes need no special-casing.
func newDialer(remote string, dial dialFunc, onDialed func(ref transport.LegRef)) *dialer {
	return &dialer{
		remote:   remote,
		dial:     dial,
		onDialed: onDialed,
		wake:     make(chan struct{}, 1),
	}
}

// redial requests a (re)dial attempt. Safe to call from the leg-down path; it
// never blocks (the wake channel coalesces duplicate requests).
func (d *dialer) redial() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// start runs the dial loop in the caller's goroutine until ctx is done. It dials
// immediately, then on each failure backs off per dialBackoffSteps. A successful
// dial calls onDialed and then parks until redial() is signaled (the leg asks for
// a fresh connection after TCP dies). Callers run this as `go d.start(ctx)`.
func (d *dialer) start(ctx context.Context) {
	if d == nil || d.dial == nil {
		return // UDP-only lane: nothing to dial.
	}

	step := 0
	for {
		ref, err := d.dial(ctx, d.remote)
		if err == nil {
			debuglog.Printf("send/dialer", "dial ok remote=%s conn=%s", d.remote, ref.ConnID)
			step = 0
			if d.onDialed != nil {
				d.onDialed(ref)
			}
			// Park until a redial is requested or ctx ends.
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
				continue
			}
		}

		debuglog.Printf("send/dialer", "dial err remote=%s err=%v backoff=%s", d.remote, err, d.backoff(step))
		wait := d.backoff(step)
		if step < len(dialBackoffSteps)-1 {
			step++
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-d.wake:
			// Explicit redial request: reset backoff and retry now.
			timer.Stop()
			step = 0
		case <-timer.C:
		}
	}
}

func (d *dialer) backoff(step int) time.Duration {
	if step >= len(dialBackoffSteps) {
		step = len(dialBackoffSteps) - 1
	}
	return dialBackoffSteps[step]
}
