package systemdbus

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call is one method call as the fake bus saw it.
type call struct {
	serial                                      uint32
	path, iface, member, destination, signature string
	body                                        []byte
}

// fakeBus accepts one client, checks its EXTERNAL authentication, and answers each call with
// reply(call): a nil error name returns an empty method return.
func fakeBus(t *testing.T, reply func(call) (errorName string)) (string, <-chan call) {
	t.Helper()
	// A short directory: a test's own temporary folder can pass the Mac's 104-byte socket limit.
	dir, err := os.MkdirTemp("", "bus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	calls := make(chan call, 8)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		auth, _ := reader.ReadString('\n')
		if auth != "\x00AUTH EXTERNAL "+hex.EncodeToString([]byte("1234"))+"\r\n" {
			io.WriteString(conn, "REJECTED EXTERNAL\r\n")
			return
		}
		io.WriteString(conn, "OK 0123456789abcdef0123456789abcdef\r\n")
		if begin, _ := reader.ReadString('\n'); begin != "BEGIN\r\n" {
			return
		}
		for {
			c, err := readCall(reader)
			if err != nil {
				return
			}
			calls <- c
			name := ""
			if c.member != "Hello" {
				name = reply(c)
			}
			conn.Write(replyMessage(c.serial, name))
		}
	}()
	return socket, calls
}

// readCall decodes a little-endian method call independently of the client's own decoder.
func readCall(r *bufio.Reader) (call, error) {
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return call{}, err
	}
	if fixed[0] != 'l' || fixed[1] != typeCall || fixed[3] != 1 {
		return call{}, errors.New("not a little-endian method call")
	}
	bodyLength, fieldsLength := binary.LittleEndian.Uint32(fixed[4:]), binary.LittleEndian.Uint32(fixed[12:])
	rest := make([]byte, int((fieldsLength+7)&^7)+int(bodyLength))
	if _, err := io.ReadFull(r, rest); err != nil {
		return call{}, err
	}
	message := append(fixed, rest...)
	c := call{serial: binary.LittleEndian.Uint32(fixed[8:]), body: message[16+int((fieldsLength+7)&^7):]}
	at := 16
	for at < 16+int(fieldsLength) {
		at = (at + 7) &^ 7
		code, sig := message[at], string(message[at+2:at+2+int(message[at+1])])
		at += 3 + int(message[at+1])
		var value string
		if sig == "g" {
			n := int(message[at])
			value, at = string(message[at+1:at+1+n]), at+2+n
		} else {
			at = (at + 3) &^ 3
			n := int(binary.LittleEndian.Uint32(message[at:]))
			value, at = string(message[at+4:at+4+n]), at+5+n
		}
		switch code {
		case 1:
			c.path = value
		case 2:
			c.iface = value
		case 3:
			c.member = value
		case 6:
			c.destination = value
		case 8:
			c.signature = value
		}
	}
	return c, nil
}

// replyMessage is a method return, or an error carrying one string message.
func replyMessage(serial uint32, errorName string) []byte {
	var e encoder
	kind, body := byte(typeReturn), []byte(nil)
	if errorName != "" {
		kind = typeError
		var b encoder
		b.string("refused")
		body = b.buf.Bytes()
	}
	e.buf.Write([]byte{'l', kind, 1, 1})
	e.uint32(uint32(len(body)))
	e.uint32(1000 + serial)
	e.array(func() {
		e.align(8)
		e.byte(5)
		e.signature("u")
		e.uint32(serial)
		if errorName != "" {
			e.align(8)
			e.byte(4)
			e.signature("s")
			e.string(errorName)
			e.align(8)
			e.byte(8)
			e.signature("g")
			e.signature("s")
		}
	})
	e.align(8)
	return append(e.buf.Bytes(), body...)
}

func dial(t *testing.T, reply func(call) string) (*Conn, <-chan call) {
	t.Helper()
	socket, calls := fakeBus(t, reply)
	conn, err := Dial(socket, 1234)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if hello := <-calls; hello.member != "Hello" || hello.destination != "org.freedesktop.DBus" || hello.path != "/org/freedesktop/DBus" {
		t.Fatalf("first call = %+v, want Hello", hello)
	}
	return conn, calls
}

func TestStartSliceSendsStartTransientUnit(t *testing.T) {
	conn, calls := dial(t, func(call) string { return "" })
	if err := conn.StartSlice("a.slice", "ddddd"); err != nil {
		t.Fatal(err)
	}
	got := <-calls
	if got.member != "StartTransientUnit" || got.iface != "org.freedesktop.systemd1.Manager" || got.path != "/org/freedesktop/systemd1" || got.destination != "org.freedesktop.systemd1" || got.signature != "ssa(sv)a(sa(sv))" {
		t.Fatalf("call = %+v", got)
	}
	// Derived by hand from the D-Bus specification: "a.slice", "fail", one (sv) struct at an
	// 8-byte boundary holding ("Description", <"ddddd">), then an empty a(sa(sv)) whose length word
	// (at 64) is followed by padding to 72, the boundary its structs would start on.
	want := []byte{
		7, 0, 0, 0, 'a', '.', 's', 'l', 'i', 'c', 'e', 0,
		4, 0, 0, 0, 'f', 'a', 'i', 'l', 0, 0, 0, 0,
		30, 0, 0, 0, 0, 0, 0, 0,
		11, 0, 0, 0, 'D', 'e', 's', 'c', 'r', 'i', 'p', 't', 'i', 'o', 'n', 0,
		1, 's', 0, 0, 5, 0, 0, 0, 'd', 'd', 'd', 'd', 'd', 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(got.body, want) {
		t.Fatalf("body\n got % x\nwant % x", got.body, want)
	}
}

func TestStopSliceTreatsAnUnknownSliceAsStopped(t *testing.T) {
	names := map[string]string{"gone.slice": "org.freedesktop.systemd1.NoSuchUnit", "busy.slice": "org.freedesktop.DBus.Error.AccessDenied"}
	conn, calls := dial(t, func(c call) string { return names[strings.TrimRight(string(c.body[4:4+c.body[0]]), "\x00")] })
	if err := conn.StopSlice("gone.slice"); err != nil {
		t.Fatalf("unknown slice: %v", err)
	}
	if got := <-calls; got.member != "StopUnit" || got.signature != "ss" {
		t.Fatalf("call = %+v", got)
	}
	err := conn.StopSlice("busy.slice")
	var reply *Error
	if !errors.As(err, &reply) || reply.Name != "org.freedesktop.DBus.Error.AccessDenied" || reply.Message != "refused" {
		t.Fatalf("refusal = %v", err)
	}
}

func TestDialFailsWhenTheBusRejectsAuthentication(t *testing.T) {
	socket, _ := fakeBus(t, func(call) string { return "" })
	if _, err := Dial(socket, 1); err == nil || !strings.Contains(err.Error(), "refused authentication") {
		t.Fatalf("Dial = %v", err)
	}
}

func TestSliceNamesAreRestricted(t *testing.T) {
	conn, _ := dial(t, func(call) string { return "" })
	for _, name := range []string{"x.service", ".slice", "-a.slice", "a-.slice", "a--b.slice", "A.slice", "a/b.slice", "a b.slice", strings.Repeat("a", 250) + ".slice"} {
		if conn.StartSlice(name, "d") == nil || conn.StopSlice(name) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}
