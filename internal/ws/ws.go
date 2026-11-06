// Package ws is a minimal RFC 6455 WebSocket implementation (server accept +
// client framing) so KubeLamp needs no third-party dependency for terminals.
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const (
	OpText   = 1
	OpBinary = 2
	OpClose  = 8
	OpPing   = 9
	OpPong   = 10
)

const maxMessage = 16 << 20

type Conn struct {
	rw     io.ReadWriteCloser
	br     *bufio.Reader
	client bool // client frames must be masked
	wmu    sync.Mutex
	closed bool
}

// NewClient wraps an already-upgraded client connection.
func NewClient(rwc io.ReadWriteCloser) *Conn {
	return &Conn{rw: rwc, br: bufio.NewReaderSize(rwc, 32<<10), client: true}
}

// Accept upgrades an incoming HTTP request. protocols lists accepted subprotocols.
func Accept(w http.ResponseWriter, r *http.Request, protocols ...string) (*Conn, string, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, "", errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, "", errors.New("missing key")
	}
	proto := ""
	for _, offered := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		offered = strings.TrimSpace(offered)
		for _, p := range protocols {
			if offered == p && proto == "" {
				proto = p
			}
		}
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return nil, "", errors.New("hijack unsupported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, "", err
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(h[:]) + "\r\n"
	if proto != "" {
		resp += "Sec-WebSocket-Protocol: " + proto + "\r\n"
	}
	resp += "\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, "", err
	}
	return &Conn{rw: conn, br: brw.Reader}, proto, nil
}

// ReadMessage returns the next data message, answering pings transparently.
func (c *Conn) ReadMessage() (int, []byte, error) {
	var msg []byte
	op := 0
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			return 0, nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := int(hdr[0] & 0x0f)
		masked := hdr[1]&0x80 != 0
		n := uint64(hdr[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return 0, nil, errors.New("websocket message too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case OpPing:
			_ = c.write(OpPong, payload)
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.write(OpClose, nil)
			return OpClose, payload, io.EOF
		case 0: // continuation
		default:
			op = opcode
		}
		msg = append(msg, payload...)
		if fin {
			return op, msg, nil
		}
	}
}

func (c *Conn) WriteMessage(op int, data []byte) error { return c.write(op, data) }

func (c *Conn) write(op int, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|byte(op))
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch n := len(data); {
	case n < 126:
		hdr = append(hdr, maskBit|byte(n))
	case n < 1<<16:
		hdr = append(hdr, maskBit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, maskBit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	payload := data
	if c.client {
		var mask [4]byte
		_, _ = rand.Read(mask[:])
		hdr = append(hdr, mask[:]...)
		payload = make([]byte, len(data))
		for i := range data {
			payload[i] = data[i] ^ mask[i%4]
		}
	}
	if _, err := c.rw.Write(append(hdr, payload...)); err != nil {
		return err
	}
	if op == OpClose {
		c.closed = true
	}
	return nil
}

func (c *Conn) Close() error {
	_ = c.write(OpClose, nil)
	return c.rw.Close()
}
