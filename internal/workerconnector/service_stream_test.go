package workerconnector

import (
	"errors"
	"io"
	"testing"
)

func TestServiceStreamRequiresFINForCleanEOF(t *testing.T) {
	for _, fin := range []bool{false, true} {
		s := &serviceSocket{received: make(chan []byte, 1), done: make(chan struct{})}
		if fin {
			s.received <- nil
		}
		close(s.done)
		_, err := s.Read(make([]byte, 1))
		want := io.ErrUnexpectedEOF
		if fin {
			want = io.EOF
		}
		if !errors.Is(err, want) {
			t.Fatalf("FIN %t: got %v, want %v", fin, err, want)
		}
	}
}
