package networkgateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// RunServiceRelay runs capless in exactly one service's network namespace.
// Even an abandoned Docker attach loses its connection at the hard deadline.
func RunServiceRelay(ctx context.Context, port string, input io.Reader, output io.Writer) error {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		return Failure("service_port_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	socket, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return Failure("service_unavailable")
	}
	defer socket.Close()
	done := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(socket, input)
		done <- errors.Join(copyErr, socket.(*net.TCPConn).CloseWrite())
	}()
	go func() { done <- writeServiceFrames(output, socket) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				return Failure("service_stream_closed")
			}
		case <-ctx.Done():
			return Failure("service_stream_expired")
		}
	}
	return nil
}

// Docker attach's stdout stays open as long as stdin. A zero-sized frame carries
// the target's write-half EOF while it continues reading a request body.
func writeServiceFrames(dst io.Writer, src io.Reader) error {
	var buffer [32 << 10]byte
	for {
		n, err := src.Read(buffer[:])
		if n > 0 {
			if writeErr := binary.Write(dst, binary.BigEndian, uint32(n)); writeErr != nil {
				return writeErr
			}
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return binary.Write(dst, binary.BigEndian, uint32(0))
		}
		if err != nil {
			return err
		}
	}
}

// ReadServiceFrames checks the helper's output before it reaches the client.
func ReadServiceFrames(dst io.Writer, halfClose func() error, src io.Reader) error {
	var buffer [32 << 10]byte
	for {
		var n uint32
		if err := binary.Read(src, binary.BigEndian, &n); err != nil {
			return err
		}
		if n == 0 {
			return halfClose()
		}
		if n > uint32(len(buffer)) {
			return errors.New("invalid service relay frame")
		}
		if _, err := io.ReadFull(src, buffer[:n]); err != nil {
			return err
		}
		if _, err := dst.Write(buffer[:n]); err != nil {
			return err
		}
	}
}
