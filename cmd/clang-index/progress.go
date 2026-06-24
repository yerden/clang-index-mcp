package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yerden/clang-index-mcp/internal/clangdproc"
)

// progressBar renders a single-line two-phase bar (clangd background
// index, then per-TU extraction) to stderr. Updates in place when
// stderr is a terminal; degrades to silent on pipes/files so build
// output stays grep-friendly. Safe for concurrent calls.
//
// The indexing phase often arrives without a usable percentage (clangd
// omits the field on report events); when pct < 0 we render an
// animated scanner driven by a background ticker so the bar still
// looks alive, alongside whatever message clangd has emitted (typically
// "N/M" once shards start completing).
type progressBar struct {
	w        io.Writer
	tty      bool
	mu       sync.Mutex
	phase    barPhase
	pct      int // -1 = unknown (indeterminate)
	message  string
	done     int
	total    int
	started  time.Time
	lastDraw time.Time
	tick     int // for the indeterminate scanner animation
	doneFlag bool
	stopTick chan struct{}
}

type barPhase int

const (
	phaseNone barPhase = iota
	phaseIndex
	phaseExtract
)

func newProgressBar() *progressBar {
	b := &progressBar{
		w:        os.Stderr,
		tty:      isTerminal(os.Stderr),
		started:  time.Now(),
		pct:      -1,
		stopTick: make(chan struct{}),
	}
	if b.tty {
		go b.animate()
	}
	return b
}

// animate repaints every 120 ms while the bar is alive. This is what
// makes the indeterminate scanner move while we're waiting on a slow
// clangd report event, and what advances the elapsed-time counter
// even when no callback is firing.
func (b *progressBar) animate() {
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.stopTick:
			return
		case <-t.C:
			b.mu.Lock()
			b.tick++
			b.draw(true)
			b.mu.Unlock()
		}
	}
}

// reportIndex is the OnIndexProgress callback. Ignored once the
// extraction phase has begun — stray late-arriving progress events
// from clangd shouldn't repaint over the new phase.
func (b *progressBar) reportIndex(ev clangdproc.IndexProgress) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.phase > phaseIndex {
		return
	}
	b.phase = phaseIndex
	if ev.Percent >= 0 && ev.Percent > b.pct {
		b.pct = ev.Percent
	}
	if ev.Message != "" {
		b.message = ev.Message
	}
	if b.pct == 100 {
		// At completion, drop any stale "N/M" trail so the final
		// paint is unambiguously 100%.
		b.message = ""
	}
	b.draw(false)
}

// reportExtract is the OnTUProgress callback.
func (b *progressBar) reportExtract(done, total int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.phase != phaseExtract {
		b.phase = phaseExtract
		b.message = ""
	}
	b.done, b.total = done, total
	b.draw(false)
}

// Finish clears the bar so subsequent output starts on a clean line.
func (b *progressBar) Finish() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.doneFlag {
		return
	}
	b.doneFlag = true
	close(b.stopTick)
	if b.tty && b.phase != phaseNone {
		fmt.Fprint(b.w, "\r\x1b[2K")
	}
}

func (b *progressBar) draw(force bool) {
	if !b.tty || b.doneFlag {
		return
	}
	now := time.Now()
	if !force && now.Sub(b.lastDraw) < 80*time.Millisecond {
		return
	}
	b.lastDraw = now

	var label, bar, trail string
	switch b.phase {
	case phaseIndex:
		label = "indexing  "
		if b.pct < 0 {
			bar = renderScanner(b.tick, 30)
			trail = "…"
		} else {
			bar = renderBar(float64(b.pct)/100, 30)
			trail = fmt.Sprintf("%3d%%", b.pct)
		}
	case phaseExtract:
		label = "extracting"
		var frac float64
		if b.total > 0 {
			frac = float64(b.done) / float64(b.total)
		}
		bar = renderBar(frac, 30)
		trail = fmt.Sprintf("%d/%d", b.done, b.total)
	default:
		return
	}
	elapsed := now.Sub(b.started).Round(time.Second)
	msg := ""
	if b.message != "" {
		msg = " · " + b.message
	}
	fmt.Fprintf(b.w, "\r\x1b[2K%s %s %s · %s%s", label, bar, trail, elapsed, msg)
}

// renderBar draws a filled bar using Unicode block characters for
// sub-cell precision.
func renderBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	subBlocks := []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}
	total := frac * float64(width)
	full := int(total)
	rem := total - float64(full)
	var b strings.Builder
	b.WriteRune('[')
	for i := 0; i < full; i++ {
		b.WriteRune('█')
	}
	if full < width {
		idx := int(rem * float64(len(subBlocks)))
		if idx >= len(subBlocks) {
			idx = len(subBlocks) - 1
		}
		b.WriteRune(subBlocks[idx])
		for i := full + 1; i < width; i++ {
			b.WriteRune(' ')
		}
	}
	b.WriteRune(']')
	return b.String()
}

// renderScanner draws an indeterminate bar — a fixed-width "block"
// that ping-pongs across the track based on tick. Used when we have
// no percentage to display.
func renderScanner(tick, width int) string {
	const block = 5
	period := 2 * (width - block)
	if period <= 0 {
		period = 1
	}
	t := tick % period
	pos := t
	if pos > width-block {
		pos = period - t
	}
	var b strings.Builder
	b.WriteRune('[')
	for i := 0; i < width; i++ {
		if i >= pos && i < pos+block {
			b.WriteRune('█')
		} else {
			b.WriteRune(' ')
		}
	}
	b.WriteRune(']')
	return b.String()
}

// isTerminal reports whether f is a character device — good enough to
// gate carriage-return repaints without pulling in golang.org/x/term.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
