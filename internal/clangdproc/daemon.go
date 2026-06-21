package clangdproc

import (
	"context"
	"sync"
	"time"
)

// Daemon adds restart-on-watch on top of Process. It owns one Process at
// a time; when Restart is called (typically by a compdb-change watcher
// elsewhere) it tears the old one down and brings a new one up against
// the same Options.
//
// Per architecture §6.1: restart over notify — we don't implement
// workspace/didChangeWatchedFiles in either direction. Restart is also
// debounced so a burst of compdb writes coalesces into one restart.
type Daemon struct {
	opts     Options
	debounce time.Duration

	mu      sync.Mutex
	current *Process

	restartCh chan struct{}
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewDaemon constructs but does not start the daemon. Call Run to bring
// up the first clangd; call Restart() to trigger reindex after compdb
// changes; call Close to tear everything down.
func NewDaemon(opts Options, debounce time.Duration) *Daemon {
	if debounce <= 0 {
		debounce = 5 * time.Second
	}
	return &Daemon{
		opts:      opts,
		debounce:  debounce,
		restartCh: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Run brings up clangd and blocks until Close, restarting on every
// debounced Restart() pulse. Each restart calls onReady once the new
// clangd is initialized and indexed (or onReady's ctx fires).
func (d *Daemon) Run(ctx context.Context, onReady func(p *Process) error) error {
	defer close(d.doneCh)

	if err := d.start(ctx, onReady); err != nil {
		return err
	}

	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			d.stopCurrent(context.Background())
			return ctx.Err()
		case <-d.stopCh:
			d.stopCurrent(context.Background())
			return nil
		case <-d.restartCh:
			if timer == nil {
				timer = time.NewTimer(d.debounce)
			} else {
				timer.Reset(d.debounce)
			}
		case <-timerC(timer):
			timer = nil
			d.stopCurrent(context.Background())
			if err := d.start(ctx, onReady); err != nil {
				return err
			}
		}
	}
}

// timerC returns the timer channel if t is non-nil, else a nil channel
// (which blocks forever in select).
func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func (d *Daemon) start(ctx context.Context, onReady func(p *Process) error) error {
	p, err := Start(ctx, d.opts)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.current = p
	d.mu.Unlock()
	if onReady != nil {
		if err := onReady(p); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) stopCurrent(ctx context.Context) {
	d.mu.Lock()
	p := d.current
	d.current = nil
	d.mu.Unlock()
	if p != nil {
		_ = p.Stop(ctx)
	}
}

// Restart asks the daemon to recycle clangd. Non-blocking: the request
// is coalesced with any pending restart inside the debounce window.
func (d *Daemon) Restart() {
	select {
	case d.restartCh <- struct{}{}:
	default:
		// already pending — debounce will handle it
	}
}

// Current returns the live Process, or nil while a restart is in flight.
func (d *Daemon) Current() *Process {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

// Close signals Run to exit.
func (d *Daemon) Close() {
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
	<-d.doneCh
}
