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
	"strings"
	"time"
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

	// Read waits for that many text messages on Conn and compares them as a
	// JSON array. A ping from the server is answered and not counted; a
	// close frame or a timeout before the count is a harness failure.
	Read int

	// Ping sends a ping with a fixed payload and waits for the pong; the
	// body is "pong <payload>".
	Ping bool

	// Close sends a close frame with code 1000 and waits for the server's;
	// the body is "close <code>" (or "close" for an empty payload), then
	// "<closed>" when the server closed the TCP connection.
	Close bool
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
		if _, ok := conns[name]; ok {
			return res, fmt.Errorf("connection %q is already open", name)
		}
		body, c, err := wsOpen(ctx, t, ws, vars)
		if err != nil {
			return res, err
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
		res.header.Set("Content-Type", "application/json")
		res.body = []byte("[" + strings.Join(msgs, ",") + "]")
	case ws.Ping:
		if err := c.writeFrame(0x9, []byte("contract-ping")); err != nil {
			return res, err
		}
		payload, err := c.readUntil(0xA)
		if err != nil {
			return res, err
		}
		res.body = []byte("pong " + string(payload))
	case ws.Close:
		delete(conns, name)
		defer c.conn.Close()
		if err := c.writeFrame(0x8, []byte{0x03, 0xE8}); err != nil {
			return res, err
		}
		payload, err := c.readUntil(0x8)
		if err != nil {
			return res, err
		}
		out := "close"
		if len(payload) >= 2 {
			out = fmt.Sprintf("close %d", binary.BigEndian.Uint16(payload))
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

func wsOpen(ctx context.Context, t *Target, ws *WSStep, vars map[string]string) ([]byte, *wsConn, error) {
	base, err := url.Parse(t.BaseURL)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, target, base.Host)
	for _, h := range ws.Header {
		v, err := expand(h[1], vars)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&req, "%s: %s\r\n", h[0], v)
	}
	req.WriteString("\r\n")

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", base.Host)
	if err != nil {
		return nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(wsTimeout))
	if _, err := conn.Write(req.Bytes()); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	var head bytes.Buffer
	for !bytes.HasSuffix(head.Bytes(), []byte("\r\n\r\n")) {
		b, err := br.ReadByte()
		if err != nil {
			conn.Close()
			if isTimeout(err) {
				return nil, nil, fmt.Errorf("no response head within %s (read %q)", wsTimeout, head.String())
			}
			// Closed (EOF or reset) before a full head.
			return append(head.Bytes(), []byte("<closed>")...), nil, nil
		}
		head.WriteByte(b)
	}
	if strings.HasPrefix(head.String(), "HTTP/1.1 101 ") {
		conn.SetDeadline(time.Time{})
		return head.Bytes(), &wsConn{conn: conn, r: br}, nil
	}
	defer conn.Close()
	rest, err := io.ReadAll(br)
	if err != nil && isTimeout(err) {
		return nil, nil, fmt.Errorf("server kept the connection open after %q", head.String())
	}
	return append(append(head.Bytes(), rest...), []byte("<closed>")...), nil, nil
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

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

// readFrame reads one frame from the server.
func (c *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.r, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.r, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.r, mask[:]); err != nil {
			return
		}
	}
	if n > 16<<20 {
		err = fmt.Errorf("frame of %d bytes", n)
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// readMessages collects n text messages, answering pings on the way. Each
// must be JSON, as every message the servers send is.
func (c *wsConn) readMessages(n int) ([]string, error) {
	var msgs []string
	var partial []byte
	for len(msgs) < n {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return msgs, fmt.Errorf("after %d of %d messages: %w", len(msgs), n, err)
		}
		switch opcode {
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return msgs, err
			}
			continue
		case 0xA:
			continue
		case 0x8:
			return msgs, fmt.Errorf("server closed after %d of %d messages (%x)", len(msgs), n, payload)
		case 0x1, 0x0:
			partial = append(partial, payload...)
		default:
			return msgs, fmt.Errorf("unexpected opcode %d", opcode)
		}
		if !fin {
			continue
		}
		if !json.Valid(partial) {
			return msgs, fmt.Errorf("message is not JSON: %q", partial)
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
		if op == 0x9 {
			if err := c.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		}
		if op == 0x8 {
			return nil, fmt.Errorf("server closed while waiting for opcode %d", opcode)
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
