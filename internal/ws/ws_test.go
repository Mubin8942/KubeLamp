package ws

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round-trips text/binary messages (incl. a large fragment-free one) between Accept and NewClient
func TestRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, proto, err := Accept(w, r, "v1")
		if err != nil {
			return
		}
		defer c.Close()
		if proto != "v1" {
			c.WriteMessage(OpText, []byte("bad proto "+proto))
			return
		}
		for {
			op, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteMessage(op, append([]byte("echo:"), m...))
		}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 16)
	rand.Read(k)
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: v0, v1\r\nSec-WebSocket-Key: "+base64.StdEncoding.EncodeToString(k)+"\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Protocol") != "v1" {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	c := &Conn{rw: conn, br: br, client: true}
	big := bytes.Repeat([]byte("x"), 200000)
	for _, m := range [][]byte{[]byte("hi"), make([]byte, 300), big} {
		if err := c.WriteMessage(OpBinary, m); err != nil {
			t.Fatal(err)
		}
		_, got, err := c.ReadMessage()
		if err != nil || !bytes.Equal(got, append([]byte("echo:"), m...)) {
			t.Fatalf("echo mismatch len %d err %v", len(got), err)
		}
	}
}
