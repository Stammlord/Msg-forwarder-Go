package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsMaxFragments  = 64
	wsMaxMessageAge = 60 * time.Second
)

type wsConn struct {
	conn       net.Conn
	br         *bufio.Reader
	writeMu    sync.Mutex
	maxMessage int

	// MAX ping guard: a server that echoes our ping must not drive an
	// endless ping-pong loop.
	pingMu       sync.Mutex
	lastPingSend time.Time
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsMagic))
	return base64.StdEncoding.EncodeToString(h[:])
}

// dialWebSocket performs a minimal RFC 6455 handshake using only stdlib.
// ALPN is pinned to http/1.1: MAX's edge does not accept WS over h2.
func dialWebSocket(rawURL, userAgent, origin string, timeout time.Duration) (result *wsConn, err error) {
	start := time.Now()
	hostName := ""
	defer func() {
		if err != nil {
			debugf("[WS] dial fail host=%s dur=%s err=%s", hostName, time.Since(start), safeErrText(err))
		} else {
			debugf("[WS] dial ok host=%s dur=%s", hostName, time.Since(start))
		}
	}()
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "wss" && u.Scheme != "ws" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	hostName = u.Hostname()
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	var conn net.Conn
	if u.Scheme == "wss" {
		td := tls.Dialer{
			NetDialer: d,
			Config: &tls.Config{
				ServerName: u.Hostname(),
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"http/1.1"},
			},
		}
		conn, err = td.Dial("tcp", host)
	} else {
		conn, err = d.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}

	// Header values must not inject extra headers.
	headerSafe := func(s string) string {
		return strings.NewReplacer("\r", "", "\n", "").Replace(s)
	}
	userAgent = headerSafe(userAgent)
	origin = headerSafe(origin)

	var sb strings.Builder
	sb.WriteString("GET ")
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\n")
	sb.WriteString("Host: ")
	sb.WriteString(u.Host)
	sb.WriteString("\r\n")
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	sb.WriteString("Sec-WebSocket-Key: ")
	sb.WriteString(key)
	sb.WriteString("\r\n")
	sb.WriteString("Sec-WebSocket-Version: 13\r\n")
	sb.WriteString("Origin: ")
	sb.WriteString(origin)
	sb.WriteString("\r\n")
	sb.WriteString("User-Agent: ")
	sb.WriteString(userAgent)
	sb.WriteString("\r\n")
	sb.WriteString("Accept: */*\r\n\r\n")

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	// A hostile endpoint could stream megabytes without a newline. Bound the
	// whole handshake to 16 KiB; br stays usable for frames afterwards.
	hr := bufio.NewReader(io.LimitReader(br, 16<<10))
	status, err := hr.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	parts := strings.Fields(status)
	if len(parts) > 1 {
		debugf("[WS] upgrade status=%s", parts[1])
	}
	if len(parts) < 2 || parts[1] != "101" {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", strings.TrimSpace(status))
	}

	var accept, upgrade, connection string
	for {
		line, err := hr.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			k := strings.ToLower(strings.TrimSpace(line[:i]))
			v := strings.TrimSpace(line[i+1:])
			switch k {
			case "sec-websocket-accept":
				accept = v
			case "upgrade":
				upgrade = v
			case "connection":
				connection = v
			}
		}
	}
	if accept != wsAccept(key) || !strings.EqualFold(upgrade, "websocket") ||
		!strings.Contains(strings.ToLower(connection), "upgrade") {
		conn.Close()
		return nil, errors.New("invalid websocket upgrade response")
	}

	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br, maxMessage: 512 << 10}, nil
}

func (w *wsConn) close() {
	_ = w.writeFrame(0x8, nil)
	_ = w.conn.Close()
}

// writeFrame sends a client frame. Client frames MUST be masked.
func (w *wsConn) writeFrame(opcode byte, payload []byte) (err error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	start := time.Now()
	defer func() {
		if err != nil {
			debugf("[WS] write fail opcode=%d bytes=%d dur=%s err=%s", opcode, len(payload), time.Since(start), safeErrText(err))
		}
	}()

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
	case n <= 0xffff:
		hdrLen += 2
	default:
		hdrLen += 8
	}
	buf := make([]byte, hdrLen+4+n)
	buf[0] = 0x80 | opcode
	switch hdrLen {
	case 2:
		buf[1] = 0x80 | byte(n)
	case 4:
		buf[1] = 0x80 | 126
		binary.BigEndian.PutUint16(buf[2:4], uint16(n))
	default:
		buf[1] = 0x80 | 127
		binary.BigEndian.PutUint64(buf[2:10], uint64(n))
	}
	copy(buf[hdrLen:hdrLen+4], mask[:])
	for i, b := range payload {
		buf[hdrLen+4+i] = b ^ mask[i&3]
	}

	_ = w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = writeAll(w.conn, buf)
	_ = w.conn.SetWriteDeadline(time.Time{})
	return err
}

// readMessage reads one (possibly fragmented) text/binary message.
// Control frames are handled internally; ping is replied to with pong.
func (w *wsConn) readMessage() ([]byte, error) {
	return w.readMessageTimeout(0)
}

// readMessageTimeout applies one absolute deadline to the whole message:
// keepalive pings and continuation frames cannot extend it beyond
// wsMaxMessageAge. A positive timeout shortens that window.
func (w *wsConn) readMessageTimeout(timeout time.Duration) (data []byte, err error) {
	start := time.Now()
	defer func() {
		if err != nil {
			debugf("[WS] read fail bytes=%d dur=%s err=%s", len(data), time.Since(start), safeErrText(err))
		} else {
			debugf("[WS] read ok bytes=%d dur=%s", len(data), time.Since(start))
		}
	}()
	deadline := start.Add(wsMaxMessageAge)
	if timeout > 0 && timeout < wsMaxMessageAge {
		deadline = start.Add(timeout)
	}
	if w.conn != nil {
		_ = w.conn.SetReadDeadline(deadline)
		defer func() { _ = w.conn.SetReadDeadline(time.Time{}) }()
	}

	var msg []byte
	fragmented := false
	fragments := 0
	for {
		if time.Since(start) > wsMaxMessageAge {
			return nil, errors.New("websocket: message assembly timeout")
		}
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		n := int(h[1] & 0x7f)

		if h[0]&0x70 != 0 {
			return nil, errors.New("websocket: reserved bits set")
		}
		if masked {
			// RFC 6455: a server MUST NOT mask frames.
			return nil, errors.New("websocket: masked server frame")
		}

		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			n = int(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			large := binary.BigEndian.Uint64(b[:])
			if large > uint64(w.maxMessage) {
				return nil, errors.New("websocket frame too large")
			}
			n = int(large)
		}
		if n < 0 || n > w.maxMessage {
			return nil, errors.New("websocket frame too large")
		}
		if opcode >= 0x8 && (!fin || n > 125) {
			return nil, errors.New("websocket: invalid control frame")
		}

		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(w.br, payload); err != nil {
				return nil, err
			}
		}

		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping
			if err := w.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2:
			if opcode != 0 && fragmented {
				return nil, errors.New("unexpected data frame during fragmentation")
			}
			if opcode == 0 && !fragmented {
				return nil, errors.New("websocket: unexpected continuation frame")
			}
			if len(msg)+n > w.maxMessage {
				return nil, errors.New("websocket message too large")
			}
			if opcode == 0 {
				fragments++
				if fragments > wsMaxFragments {
					return nil, errors.New("websocket: too many fragments")
				}
				msg = append(msg, payload...)
				fragmented = true
			} else {
				msg = payload
				fragmented = !fin
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
	}
}
