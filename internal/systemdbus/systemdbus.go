// Package systemdbus starts and stops the transient systemd slices that hold
// kernel filesystem protection on hosts whose Docker uses the systemd cgroup
// driver. Docker there accepts only a slice as a parent, and systemd owns every
// slice; Coop asks systemd for its slice before any box exists instead of making
// cgroups behind systemd's back.
//
// It speaks the few D-Bus messages it needs over the system bus socket, so the
// helper stays a single static binary with no D-Bus library.
package systemdbus

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Socket is the system bus as the helper sees it; the host mounts it there.
const Socket = "/run/dbus/system_bus_socket"

const (
	callTimeout = 15 * time.Second
	maxMessage  = 1 << 20
)

// Conn is one authenticated system bus connection, used by one goroutine.
type Conn struct {
	conn   net.Conn
	reader *bufio.Reader
	serial uint32
}

// Error is a D-Bus error reply, such as org.freedesktop.systemd1.NoSuchUnit.
type Error struct{ Name, Message string }

func (e *Error) Error() string { return "systemd: " + e.Name + ": " + e.Message }

// Dial connects and authenticates as the calling user, then says Hello.
func Dial(path string, uid int) (*Conn, error) {
	conn, err := net.DialTimeout("unix", path, callTimeout)
	if err != nil {
		return nil, err
	}
	c := &Conn{conn: conn, reader: bufio.NewReader(conn)}
	if err := c.auth(uid); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := c.call(message{path: "/org/freedesktop/DBus", iface: "org.freedesktop.DBus", member: "Hello", destination: "org.freedesktop.DBus"}); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) Close() error { return c.conn.Close() }

func (c *Conn) auth(uid int) error {
	if err := c.conn.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return err
	}
	if _, err := io.WriteString(c.conn, "\x00AUTH EXTERNAL "+hex.EncodeToString([]byte(strconv.Itoa(uid)))+"\r\n"); err != nil {
		return err
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("system bus refused authentication: %q", strings.TrimSpace(line))
	}
	_, err = io.WriteString(c.conn, "BEGIN\r\n")
	return err
}

// StartSlice starts a transient slice. systemd creates its cgroup; the caller
// waits for that directory, since the reply only means the job was queued.
func (c *Conn) StartSlice(name, description string) error {
	if !validSlice(name) {
		return errors.New("invalid systemd slice name")
	}
	var body encoder
	body.string(name)
	body.string("fail")
	// a(sv): one property, Description.
	body.array(func() {
		body.align(8)
		body.string("Description")
		body.variantString(description)
	})
	// a(sa(sv)): no auxiliary units.
	body.array(func() {})
	_, err := c.call(message{path: "/org/freedesktop/systemd1", iface: "org.freedesktop.systemd1.Manager", member: "StartTransientUnit", destination: "org.freedesktop.systemd1", signature: "ssa(sv)a(sa(sv))", body: body.buf.Bytes()})
	return err
}

// StopSlice stops a slice. A slice systemd no longer knows counts as stopped.
// Stopping a slice also stops every unit inside it, so callers first prove it
// empty by removing its cgroup.
func (c *Conn) StopSlice(name string) error {
	if !validSlice(name) {
		return errors.New("invalid systemd slice name")
	}
	var body encoder
	body.string(name)
	body.string("fail")
	_, err := c.call(message{path: "/org/freedesktop/systemd1", iface: "org.freedesktop.systemd1.Manager", member: "StopUnit", destination: "org.freedesktop.systemd1", signature: "ss", body: body.buf.Bytes()})
	var reply *Error
	if errors.As(err, &reply) && reply.Name == "org.freedesktop.systemd1.NoSuchUnit" {
		return nil
	}
	return err
}

func validSlice(name string) bool {
	stem, ok := strings.CutSuffix(name, ".slice")
	return ok && stem != "" && len(name) <= 255 && strings.Trim(stem, "abcdefghijklmnopqrstuvwxyz0123456789_-") == "" && !strings.HasPrefix(stem, "-") && !strings.HasSuffix(stem, "-") && !strings.Contains(stem, "--")
}

type message struct {
	path, iface, member, destination, signature string
	body                                        []byte
}

const (
	typeCall   = 1
	typeReturn = 2
	typeError  = 3
)

// call sends one method call and returns its reply body, skipping signals and
// replies to anything else.
func (c *Conn) call(m message) ([]byte, error) {
	c.serial++
	serial := c.serial
	// Header fields are aligned from the start of the message, so they are
	// written after the fixed part in the same buffer.
	var header encoder
	header.buf.Write([]byte{'l', typeCall, 0, 1})
	header.uint32(uint32(len(m.body)))
	header.uint32(serial)
	header.array(func() {
		field := func(code byte, signature string, value func()) {
			header.align(8)
			header.byte(code)
			header.signature(signature)
			value()
		}
		field(1, "o", func() { header.string(m.path) })
		field(2, "s", func() { header.string(m.iface) })
		field(3, "s", func() { header.string(m.member) })
		field(6, "s", func() { header.string(m.destination) })
		if m.signature != "" {
			field(8, "g", func() { header.signature(m.signature) })
		}
	})
	header.align(8)
	if err := c.conn.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(header.buf.Bytes(), m.body...)); err != nil {
		return nil, err
	}
	for {
		kind, replyTo, name, body, order, err := c.read()
		if err != nil {
			return nil, err
		}
		if replyTo != serial || kind != typeReturn && kind != typeError {
			continue
		}
		if kind == typeError {
			text, _ := (&decoder{data: body, order: order}).string()
			return nil, &Error{Name: name, Message: text}
		}
		return body, nil
	}
}

// read returns one message's type, reply serial, error name and body. Only
// little- and big-endian framing and the header fields Coop reads are decoded.
func (c *Conn) read() (kind byte, replyTo uint32, errorName string, body []byte, order binary.ByteOrder, err error) {
	fixed := make([]byte, 16)
	if _, err = io.ReadFull(c.reader, fixed); err != nil {
		return
	}
	order = binary.LittleEndian
	switch fixed[0] {
	case 'l':
	case 'B':
		order = binary.BigEndian
	default:
		return 0, 0, "", nil, nil, errors.New("invalid D-Bus message")
	}
	kind = fixed[1]
	bodyLength, fieldsLength := uint64(order.Uint32(fixed[4:])), uint64(order.Uint32(fixed[12:]))
	padded := (fieldsLength + 7) &^ 7
	if fixed[3] != 1 || bodyLength+padded > maxMessage {
		return 0, 0, "", nil, nil, errors.New("invalid D-Bus message size")
	}
	rest := make([]byte, int(padded)+int(bodyLength))
	if _, err = io.ReadFull(c.reader, rest); err != nil {
		return
	}
	// Field offsets are counted from the start of the message.
	d := &decoder{data: append(fixed, rest[:fieldsLength]...), at: 16, order: order}
	for d.at < len(d.data) {
		d.align(8)
		code, err := d.byte()
		if err != nil {
			return 0, 0, "", nil, nil, err
		}
		signature, err := d.signature()
		if err != nil {
			return 0, 0, "", nil, nil, err
		}
		switch signature {
		case "u":
			value, err := d.uint32()
			if err != nil {
				return 0, 0, "", nil, nil, err
			}
			if code == 5 {
				replyTo = value
			}
		case "s", "o":
			value, err := d.string()
			if err != nil {
				return 0, 0, "", nil, nil, err
			}
			if code == 4 {
				errorName = value
			}
		case "g":
			if _, err := d.signature(); err != nil {
				return 0, 0, "", nil, nil, err
			}
		default:
			return 0, 0, "", nil, nil, fmt.Errorf("unexpected D-Bus header field type %q", signature)
		}
	}
	body = rest[padded:]
	return kind, replyTo, errorName, body, order, nil
}

// encoder writes little-endian D-Bus values. Alignment is relative to the start
// of the buffer: the header and the body each start on an 8-byte boundary.
type encoder struct{ buf bytes.Buffer }

func (e *encoder) align(n int) {
	for e.buf.Len()%n != 0 {
		e.buf.WriteByte(0)
	}
}
func (e *encoder) byte(b byte) { e.buf.WriteByte(b) }
func (e *encoder) uint32(v uint32) {
	e.align(4)
	_ = binary.Write(&e.buf, binary.LittleEndian, v)
}
func (e *encoder) string(s string) {
	e.uint32(uint32(len(s)))
	e.buf.WriteString(s)
	e.buf.WriteByte(0)
}
func (e *encoder) signature(s string) {
	e.buf.WriteByte(byte(len(s)))
	e.buf.WriteString(s)
	e.buf.WriteByte(0)
}
func (e *encoder) variantString(s string) {
	e.signature("s")
	e.string(s)
}

// array writes an array of 8-aligned structs; the length excludes the padding
// before the first element.
func (e *encoder) array(elements func()) {
	e.align(4)
	at := e.buf.Len()
	e.uint32(0)
	e.align(8)
	start := e.buf.Len()
	elements()
	binary.LittleEndian.PutUint32(e.buf.Bytes()[at:], uint32(e.buf.Len()-start))
}

type decoder struct {
	data  []byte
	at    int
	order binary.ByteOrder
}

func (d *decoder) align(n int) { d.at = (d.at + n - 1) &^ (n - 1) }
func (d *decoder) byte() (byte, error) {
	if d.at >= len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	d.at++
	return d.data[d.at-1], nil
}
func (d *decoder) uint32() (uint32, error) {
	d.align(4)
	if d.at+4 > len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	if d.order == nil {
		d.order = binary.LittleEndian
	}
	d.at += 4
	return d.order.Uint32(d.data[d.at-4:]), nil
}
func (d *decoder) string() (string, error) {
	n, err := d.uint32()
	if err != nil {
		return "", err
	}
	if uint64(d.at)+uint64(n)+1 > uint64(len(d.data)) {
		return "", io.ErrUnexpectedEOF
	}
	s := string(d.data[d.at : d.at+int(n)])
	d.at += int(n) + 1
	return s, nil
}
func (d *decoder) signature() (string, error) {
	n, err := d.byte()
	if err != nil {
		return "", err
	}
	if d.at+int(n)+1 > len(d.data) {
		return "", io.ErrUnexpectedEOF
	}
	s := string(d.data[d.at : d.at+int(n)])
	d.at += int(n) + 1
	return s, nil
}
