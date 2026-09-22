//go:build providerlivee2e

package loop

import (
	"errors"
	"io"
)

// LiveStreamProbe qualifies the production decoder's lifecycle signals without exposing raw
// provider output or adding a production observation API. Only the live-test build includes it.
type LiveStreamProbe struct {
	decoder iterationStreamDecoder
	open    map[string]bool
	starts  int
	ends    int
	finals  int
	invalid bool
}

func NewLiveStreamProbe(provider string) *LiveStreamProbe {
	p := &LiveStreamProbe{open: map[string]bool{}}
	p.decoder = newIterationStreamDecoder(provider, io.Discard, io.Discard, io.Discard, "", "", "", nil)
	if p.decoder != nil {
		p.decoder.setActivity(p)
	}
	return p
}

func (p *LiveStreamProbe) Write(data []byte) (int, error) {
	if p.decoder == nil {
		return 0, errors.New("provider has no native stream decoder")
	}
	return p.decoder.Write(data)
}

// Verify must run after the provider's stdout writer stops, just as runIteration flushes its
// decoder after box.Run returns. Successful task-channel traffic is independent evidence.
func (p *LiveStreamProbe) Verify() error {
	if p.decoder == nil {
		return errors.New("provider has no native stream decoder")
	}
	p.decoder.flush()
	if p.invalid || p.starts == 0 || p.starts != p.ends || len(p.open) != 0 || p.finals != 1 || p.decoder.streamOutcome() != streamSucceeded {
		return errors.New("provider stream did not prove paired tools and one successful terminal event")
	}
	return nil
}

func (p *LiveStreamProbe) bootstrap() {}
func (p *LiveStreamProbe) progress()  {}

func (p *LiveStreamProbe) toolStart(id string) {
	if id == "" || p.open[id] || p.finals != 0 || len(p.open) >= maxOpenTools {
		p.invalid = true
		return
	}
	p.open[id] = true
	p.starts++
}

func (p *LiveStreamProbe) toolEnd(id string) {
	if !p.open[id] || p.finals != 0 {
		p.invalid = true
		return
	}
	delete(p.open, id)
	p.ends++
}

func (p *LiveStreamProbe) terminal() {
	p.finals++
	if len(p.open) != 0 {
		p.invalid = true
	}
}
