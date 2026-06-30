package ui

import (
	"bytes"
	"testing"
)

// Region skips a repaint that changes nothing (same content, no history, same width) — like
// AltScreen.Frame — so the loop bar doesn't flicker on a no-op poll; real changes still repaint.
func TestRegionSkipsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	r := NewRegion(&buf, func() int { return 80 })

	r.Update("", []string{"bar v1"})
	n := buf.Len()

	r.Update("", []string{"bar v1"}) // identical, no history → skip
	if buf.Len() != n {
		t.Errorf("identical Update should be skipped; wrote %d more bytes", buf.Len()-n)
	}

	r.Update("", []string{"bar v2"}) // content changed → repaint
	if buf.Len() == n {
		t.Error("a changed Update should repaint")
	}

	n = buf.Len()
	r.Update("scroll line", []string{"bar v2"}) // history present → always repaint
	if buf.Len() == n {
		t.Error("an Update with history should always repaint")
	}

	r.Clear() // a cleared bar must come back
	n = buf.Len()
	r.Update("", []string{"bar v2"})
	if buf.Len() == n {
		t.Error("after Clear, the next Update must repaint (not skip as unchanged)")
	}
}
