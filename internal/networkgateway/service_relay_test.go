package networkgateway

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestServiceRelayTargetCanHalfCloseBeforeReadingRequest(t *testing.T) {
	server, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serverRead := make(chan string, 1)
	go func() {
		conn, err := server.AcceptTCP()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("banner"))
		_ = conn.CloseWrite()
		data, _ := io.ReadAll(conn)
		serverRead <- string(data)
	}()
	input, writeInput := io.Pipe()
	defer writeInput.Close()
	output, writeOutput := io.Pipe()
	defer output.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		err := RunServiceRelay(ctx, strconv.Itoa(server.Addr().(*net.TCPAddr).Port), input, writeOutput)
		_ = writeOutput.CloseWithError(err)
		finished <- err
	}()
	var banner bytes.Buffer
	if err := ReadServiceFrames(&banner, func() error {
		_, err := io.WriteString(writeInput, "after the banner")
		_ = writeInput.Close()
		return err
	}, output); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-serverRead:
		if got != "after the banner" || banner.String() != "banner" {
			t.Fatalf("banner %q, request %q", banner.String(), got)
		}
	case <-ctx.Done():
		t.Fatal("target EOF blocked the remaining request")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
