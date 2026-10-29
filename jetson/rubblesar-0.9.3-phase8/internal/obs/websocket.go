// Package obs controls OBS Studio through obs-websocket 5 (built into OBS 28
// and later: Tools → WebSocket Server Settings), so the demo director can
// start and stop recordings on the ZBook and on the WinTAK laptop, switch
// scenes and mark chapters (phase 8). It speaks the WebSocket protocol
// (RFC 6455) itself: no third-party code.
package obs

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsConn is a client WebSocket connection (text messages only).
type wsConn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// dialWS opens a WebSocket to a ws:// URL.
func dialWS(raw string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("only ws:// addresses are supported (got %s)", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "4455")
	}
	c, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	kb := make([]byte, 16)
	_, _ = rand.Read(kb)
	key := base64.StdEncoding.EncodeToString(kb)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: obswebsocket.json\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("handshake: %s (is this obs-websocket?)", resp.Status)
	}
	h := sha1.Sum([]byte(key + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h[:]) {
		c.Close()
		return nil, errors.New("handshake: wrong Sec-WebSocket-Accept")
	}
	_ = c.SetDeadline(time.Time{})
	return &wsConn{c: c, r: br}, nil
}

// writeText sends one masked text frame (clients must mask).
func (w *wsConn) writeText(b []byte) error { return w.writeFrame(0x1, b) }

func (w *wsConn) writeFrame(op byte, b []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	hdr := []byte{0x80 | op}
	n := len(b)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	out := make([]byte, len(hdr)+n)
	copy(out, hdr)
	for i := 0; i < n; i++ {
		out[len(hdr)+i] = b[i] ^ mask[i%4]
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := w.c.Write(out)
	return err
}

// readMessage returns the next text message (answering pings, joining fragments).
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.r, h); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		masked, n := h[1]&0x80 != 0, uint64(h[1]&0x7f)
		switch n {
		case 126:
			var e [2]byte
			if _, err := io.ReadFull(w.r, e[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(e[:]))
		case 127:
			var e [8]byte
			if _, err := io.ReadFull(w.r, e[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(e[:])
		}
		if n > 64<<20 {
			return nil, errors.New("message too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.r, mask[:]); err != nil {
				return nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.r, p); err != nil {
			return nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8: // close
			return nil, parseClose(p)
		case 0x9: // ping
			if err := w.writeFrame(0xA, p); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2, 0x0:
			msg = append(msg, p...)
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("unexpected frame opcode %d", op)
		}
	}
}

func (w *wsConn) close() error {
	_ = w.writeFrame(0x8, []byte{0x03, 0xe8})
	return w.c.Close()
}

// acceptKey is the server's answer to a client key (for the test server).
func acceptKey(key string) string {
	h := sha1.Sum([]byte(strings.TrimSpace(key) + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}
