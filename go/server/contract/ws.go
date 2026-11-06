package contract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// WSStep is a WebSocket step (Step.WS). Exactly one of Open, Read, Ping and
// Close is set. Each names a connection (Conn, default "ws") of the side's
// own server; connections still open after the last step are closed.
type WSStep struct {
	Conn string

	// Open sends a raw HTTP/1.1 upgrade request on a new TCP connection:
	// "<Method> <Target> HTTP/1.1", Host, then Header in order (values may
	// use {{name}}). The step's body is the response as read: the head of a
	// 101 (the connection then stays open under Conn), or the whole
	// response up to the server's close, with "<closed>" appended. A server
	// that closes with no response gives "<closed>" alone.
	Open   bool
	Method string // default GET
	Target string // default "/admin/ws"
	Header [][2]string
	// Expect is the outcome the case is written for, checked on each side
	// on its own: an HTTP status ("101", "401", "400", ...) or "closed" for
	// no response. Required on Open: a case whose premise breaks (an
	// invalid fixture token, say) must fail, not compare two identical
	// refusals.
	Expect string

	// Read waits for that many text messages on Conn. The body is a JSON
	// array of the payloads as strings, so the bytes are compared, not only
	// the JSON they encode. Each must be valid JSON. A ping from the server
	// is answered and not counted; a close frame, a timeout or a protocol
	// violation before the count is a harness failure.
	Read int

	// Ping sends a ping with a fixed payload and waits for the pong; the
	// body is "pong <payload>".
	Ping bool

	// Close sends a close frame with CloseCode (default 1000; -1 sends an
	// empty payload) and CloseReason, and waits for the server's. The body
	// is "close" and the server's payload (its code and quoted reason), then
	// "<closed>" when the server closed the TCP connection.
	Close       bool
	CloseCode   int
	CloseReason string
}

// wsTimeout bounds one WebSocket step.
const wsTimeout = 10 * time.Second

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func wsLabel(ws *WSStep) string {
	name := ws.Conn
	if name == "" {
		name = "ws"
	}
	switch {
	case ws.Open:
		target := ws.Target
		if target == "" {
			target = "/admin/ws"
		}
		method := ws.Method
		if method == "" {
			method = http.MethodGet
		}
		return "ws " + name + " open " + method + " " + target
	case ws.Read > 0:
		return fmt.Sprintf("ws %s read %d", name, ws.Read)
	case ws.Ping:
		return "ws " + name + " ping"
	default:
		return "ws " + name + " close"
	}
}

// wsStep runs one WebSocket step against t. Every failure is a harness
// failure, for the reason request gives.
func (r *Runner) wsStep(ctx context.Context, t *Target, st Step, vars map[string]string, conns map[string]*wsConn) (rawStep, error) {
	ws := st.WS
	res := rawStep{label: wsLabel(ws), header: http.Header{}}
	if st.Label != "" {
		res.label = st.Label
	}
	name := ws.Conn
	if name == "" {
		name = "ws"
	}
	if ws.Open {
		if ws.Expect == "" {
			return res, errors.New("an Open step needs Expect")
		}
		if _, ok := conns[name]; ok {
			return res, fmt.Errorf("connection %q is already open", name)
		}
		body, outcome, c, err := wsOpen(ctx, t, ws, vars)
		if err != nil {
			return res, err
		}
		if outcome != ws.Expect {
			if c != nil {
				c.conn.Close()
			}
			return res, fmt.Errorf("expected %s, got %s: %q", ws.Expect, outcome, body)
		}
		if c != nil {
			conns[name] = c
		}
		res.body = body
		return res, nil
	}
	c, ok := conns[name]
	if !ok {
		return res, fmt.Errorf("no open connection %q", name)
	}
	c.conn.SetDeadline(time.Now().Add(wsTimeout))
	defer c.conn.SetDeadline(time.Time{})
	switch {
	case ws.Read > 0:
		msgs, err := c.readMessages(ws.Read)
		if err != nil {
			return res, err
		}
		body, err := json.Marshal(msgs)
		if err != nil {
			return res, err
		}
		res.header.Set("Content-Type", "application/json")
		res.body = body
	case ws.Ping:
		if err := c.writeFrame(opPing, []byte("contract-ping")); err != nil {
			return res, err
		}
		payload, err := c.readUntil(opPong)
		if err != nil {
			return res, err
		}
		res.body = []byte("pong " + string(payload))
	case ws.Close:
		delete(conns, name)
		defer c.conn.Close()
		var payload []byte
		switch {
		case ws.CloseCode == -1:
		case ws.CloseCode == 0:
			payload = closePayload(1000, ws.CloseReason)
		default:
			payload = closePayload(ws.CloseCode, ws.CloseReason)
		}
		if err := c.writeFrame(opClose, payload); err != nil {
			return res, err
		}
		reply, err := c.readUntil(opClose)
		if err != nil {
			return res, err
		}
		out := "close"
		switch {
		case len(reply) >= 2:
			out += fmt.Sprintf(" %d %q", binary.BigEndian.Uint16(reply), reply[2:])
		case len(reply) == 1:
			out += fmt.Sprintf(" malformed %x", reply)
		}
		if c.closedByServer() {
			out += "\n<closed>"
		}
		res.body = []byte(out)
	default:
		return res, errors.New("WSStep sets none of Open, Read, Ping, Close")
	}
	return res, nil
}

func closePayload(code int, reason string) []byte {
	p := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))
	return append(p, reason...)
}

// wsOpen sends the upgrade request. outcome is the status code of the
// response, or "closed" when there was none.
func wsOpen(ctx context.Context, t *Target, ws *WSStep, vars map[string]string) (body []byte, outcome string, c *wsConn, err error) {
	base, err := url.Parse(t.BaseURL)
	if err != nil {
		return nil, "", nil, err
	}
	method := ws.Method
	if method == "" {
		method = http.MethodGet
	}
	target := ws.Target
	if target == "" {
		target = "/admin/ws"
	}
	target, err = expand(target, vars)
	if err != nil {
		return nil, "", nil, err
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, target, base.Host)
	for _, h := range ws.Header {
		v, err := expand(h[1], vars)
		if err != nil {
			return nil, "", nil, err
		}
		fmt.Fprintf(&req, "%s: %s\r\n", h[0], v)
	}
	req.WriteString("\r\n")

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", base.Host)
	if err != nil {
		return nil, "", nil, err
	}
	conn.SetDeadline(time.Now().Add(wsTimeout))
	if _, err := conn.Write(req.Bytes()); err != nil {
		conn.Close()
		return nil, "", nil, err
	}
	br := bufio.NewReader(conn)
	var head bytes.Buffer
	for !bytes.HasSuffix(head.Bytes(), []byte("\r\n\r\n")) {
		b, err := br.ReadByte()
		if err != nil {
			conn.Close()
			if isTimeout(err) {
				return nil, "", nil, fmt.Errorf("no response head within %s (read %q)", wsTimeout, head.String())
			}
			// Closed (EOF or reset) before a full head.
			if head.Len() == 0 {
				return []byte("<closed>"), "closed", nil, nil
			}
			return append(head.Bytes(), []byte("<closed>")...), "partial", nil, nil
		}
		head.WriteByte(b)
	}
	outcome = statusOf(head.String())
	if outcome == "101" {
		conn.SetDeadline(time.Time{})
		return head.Bytes(), outcome, &wsConn{conn: conn, r: br}, nil
	}
	defer conn.Close()
	rest, err := io.ReadAll(br)
	if err != nil && isTimeout(err) {
		return nil, "", nil, fmt.Errorf("server kept the connection open after %q", head.String())
	}
	return append(append(head.Bytes(), rest...), []byte("<closed>")...), outcome, nil, nil
}

// statusOf is the status code of "HTTP/1.1 <code> <text>".
func statusOf(head string) string {
	fields := strings.SplitN(head, " ", 3)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return "malformed"
	}
	if _, err := strconv.Atoi(fields[1]); err != nil {
		return "malformed"
	}
	return fields[1]
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// writeFrame sends one final frame, masked as a client must.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		return errors.New("payload too large for the harness")
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

// readFrame reads one frame from the server and refuses what RFC 6455 does
// not allow a server to send without an extension: a masked frame, a set
// RSV bit, an unknown opcode, a fragmented or oversized control frame, a
// length in a longer form than needed.
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.r, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	if rsv := hdr[0] & 0x70; rsv != 0 {
		err = fmt.Errorf("protocol: RSV bits %#x set", rsv)
		return
	}
	opcode = hdr[0] & 0x0F
	switch opcode {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		err = fmt.Errorf("protocol: unknown opcode %#x", opcode)
		return
	}
	if hdr[1]&0x80 != 0 {
		err = errors.New("protocol: the server sent a masked frame")
		return
	}
	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
		if n < 126 {
			err = fmt.Errorf("protocol: length %d in the 16-bit form", n)
			return
		}
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
		if n <= 0xFFFF || n>>63 != 0 {
			err = fmt.Errorf("protocol: length %d in the 64-bit form", n)
			return
		}
	}
	if opcode >= opClose && (!fin || n > 125) {
		err = fmt.Errorf("protocol: control frame %#x fragmented or %d bytes", opcode, n)
		return
	}
	if n > 16<<20 {
		err = fmt.Errorf("frame of %d bytes", n)
		return
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(c.r, payload)
	return
}

// readMessages collects n text messages, answering pings on the way. Each
// must be valid UTF-8 and JSON, as every message the servers send is.
func (c *wsConn) readMessages(n int) ([]string, error) {
	var msgs []string
	var partial []byte
	inMessage := false
	for len(msgs) < n {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return msgs, fmt.Errorf("after %d of %d messages: %w", len(msgs), n, err)
		}
		switch opcode {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return msgs, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return msgs, fmt.Errorf("server closed after %d of %d messages (%x)", len(msgs), n, payload)
		case opText:
			if inMessage {
				return msgs, errors.New("protocol: a text frame inside a fragmented message")
			}
			inMessage = true
			partial = append(partial[:0], payload...)
		case opContinuation:
			if !inMessage {
				return msgs, errors.New("protocol: a continuation frame with no message in progress")
			}
			partial = append(partial, payload...)
		default:
			return msgs, fmt.Errorf("unexpected opcode %#x", opcode)
		}
		if !fin {
			continue
		}
		inMessage = false
		if !utf8.Valid(partial) || !json.Valid(partial) {
			return msgs, fmt.Errorf("message is not UTF-8 JSON: %q", partial)
		}
		msgs = append(msgs, string(partial))
		partial = nil
	}
	return msgs, nil
}

// readUntil reads frames until one of opcode arrives, answering pings and
// skipping data messages.
func (c *wsConn) readUntil(opcode byte) ([]byte, error) {
	for {
		_, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		if op == opcode {
			return payload, nil
		}
		if op == opPing {
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		}
		if op == opClose {
			return nil, fmt.Errorf("server closed while waiting for opcode %#x", opcode)
		}
	}
}

// closedByServer reports whether the server closes the TCP connection
// within a second after the close handshake.
func (c *wsConn) closedByServer() bool {
	c.conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := c.r.ReadByte()
	return err != nil && !isTimeout(err)
}
