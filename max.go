package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	maxOpcodePing        int16 = 1
	maxOpcodeInit        int16 = 6
	maxOpcodeVerify      int16 = 17
	maxOpcodeCode        int16 = 18
	maxOpcodeLogin       int16 = 19
	maxOpcodeSub         int16 = 75
	maxOpcodeCaptcha     int16 = 224
	maxProtoVersion      byte  = 10
	maxMaxFrameSize            = 512 << 10
	maxAckTimeout              = 30 * time.Second
	maxAckMaxSkipFrames        = 64
	maxPingReplyInterval       = 5 * time.Second
)

type maxFrame struct {
	ver     byte
	cmd     byte
	seq     int16
	opcode  int16
	payload any
}

// notePingSent records every outgoing MAX ping: keepalive, validation and
// replies alike. A reply to our own ping is therefore suppressed by the
// interval check below.
func (w *wsConn) notePingSent() {
	w.pingMu.Lock()
	w.lastPingSend = time.Now()
	w.pingMu.Unlock()
}

// pingReplyAllowed rate-limits replies to server pings. Without it a MAX
// server that echoes pings would create an endless ping-pong loop.
func (w *wsConn) pingReplyAllowed() bool {
	w.pingMu.Lock()
	defer w.pingMu.Unlock()
	return time.Since(w.lastPingSend) >= maxPingReplyInterval
}

func putUint24(b []byte, n int) {
	b[0] = byte(n >> 16)
	b[1] = byte(n >> 8)
	b[2] = byte(n)
}

func getUint24(b []byte) int {
	return int(b[0])<<16 | int(b[1])<<8 | int(b[2])
}

// sendMaxFrame builds the MAX binary frame: 10-byte header + msgpack payload.
// Header: ver, cmd, seq(int16 BE), opcode(int16 BE), lz4-factor, payload-len(u24 BE).
func sendMaxFrame(ws *wsConn, cmd byte, seq, opcode int16, payload any) error {
	p := mpEncode(payload)
	if len(p) > 0xffffff {
		return fmt.Errorf("max: payload too large")
	}
	frame := make([]byte, 10+len(p))
	frame[0] = maxProtoVersion
	frame[1] = cmd
	binary.BigEndian.PutUint16(frame[2:4], uint16(seq))
	binary.BigEndian.PutUint16(frame[4:6], uint16(opcode))
	frame[6] = 0 // we never compress outgoing frames
	putUint24(frame[7:10], len(p))
	copy(frame[10:], p)
	if err := ws.writeFrame(0x2, frame); err != nil {
		return err
	}
	if opcode == maxOpcodePing {
		ws.notePingSent()
	}
	debugf("[MAX] out cmd=%d seq=%d opcode=%d bytes=%d", cmd, seq, opcode, len(p))
	return nil
}

func parseMaxFrame(data []byte) (maxFrame, error) {
	var f maxFrame
	if len(data) < 10 {
		return f, fmt.Errorf("max: short frame")
	}
	if data[0] != maxProtoVersion {
		return f, fmt.Errorf("max: unsupported protocol version %d", data[0])
	}
	f.ver = data[0]
	f.cmd = data[1]
	f.seq = int16(binary.BigEndian.Uint16(data[2:4]))
	f.opcode = int16(binary.BigEndian.Uint16(data[4:6]))
	factor := data[6]
	n := getUint24(data[7:10])
	if n < 0 || 10+n > len(data) {
		return f, fmt.Errorf("max: bad payload length")
	}
	body := data[10 : 10+n]
	if factor > 0 {
		var err error
		body, err = lz4Decompress(body, maxMaxFrameSize)
		if err != nil {
			return f, err
		}
	}
	v, err := decodeMsgpack(body)
	if err != nil {
		debugf("[MAX] in frame decode fail opcode=%d err=%s", f.opcode, safeErrText(err))
		return f, err
	}
	f.payload = v
	debugf("[MAX] in cmd=%d seq=%d opcode=%d factor=%d wireBytes=%d plainBytes=%d payloadKeys=%s",
		f.cmd, f.seq, f.opcode, factor, n, len(body), mapKeys(asMap(v)))
	return f, nil
}

func waitMaxAck(ws *wsConn, seq, opcode int16) (maxFrame, error) {
	start := time.Now()
	deadline := start.Add(maxAckTimeout)
	bad := 0
	skipped := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			debugf("[MAX] ack timeout seq=%d opcode=%d skipped=%d bad=%d", seq, opcode, skipped, bad)
			return maxFrame{}, fmt.Errorf("max: ack timeout")
		}
		data, err := ws.readMessageTimeout(remaining)
		if err != nil {
			debugf("[MAX] ack read fail seq=%d opcode=%d err=%s dur=%s", seq, opcode, errKind(err), time.Since(start))
			return maxFrame{}, err
		}
		f, err := parseMaxFrame(data)
		if err != nil {
			bad++
			skipped++
			if bad > 16 || skipped > maxAckMaxSkipFrames {
				return maxFrame{}, err
			}
			continue
		}
		if f.cmd == 0 && f.opcode == maxOpcodePing {
			if ws.pingReplyAllowed() {
				_ = sendMaxFrame(ws, 0, 0, maxOpcodePing, map[string]any{"interactive": true})
			}
			skipped++
			if skipped > maxAckMaxSkipFrames {
				return maxFrame{}, fmt.Errorf("max: too many unrelated frames")
			}
			continue
		}
		if f.cmd == 3 && f.seq == seq {
			debugf("[MAX] ack error seq=%d opcode=%d code=%d dur=%s", seq, opcode, f.opcode, time.Since(start))
			return f, fmt.Errorf("max error opcode %d: %s", f.opcode, maxErrorCode(f.payload))
		}
		if f.cmd == 1 && f.seq == seq && f.opcode == opcode {
			debugf("[MAX] ack ok seq=%d opcode=%d skipped=%d dur=%s", seq, opcode, skipped, time.Since(start))
			return f, nil
		}
		skipped++
		if skipped > maxAckMaxSkipFrames {
			debugf("[MAX] ack skipped too many seq=%d opcode=%d dur=%s", seq, opcode, time.Since(start))
			return maxFrame{}, fmt.Errorf("max: too many unrelated frames")
		}
	}
}

func (a *app) maxWait(ws *wsConn, seq, opcode int16) (maxFrame, error) {
	return waitMaxAck(ws, seq, opcode)
}

type maxChat struct {
	id    int64
	typ   string
	title string
}

func firstMaxName(v any) string {
	for _, raw := range asSlice(v) {
		m := asMap(raw)
		name := asString(m["name"])
		if name == "" {
			first := strings.TrimSpace(asString(m["firstName"]))
			last := strings.TrimSpace(asString(m["lastName"]))
			name = strings.TrimSpace(first + " " + last)
		}
		if name != "" {
			return name
		}
	}
	return ""
}

func (a *app) parseMaxLogin(v any) (int64, []maxChat) {
	m := asMap(v)
	myID, _ := toInt64(nested(m, "profile", "contact", "id"))

	users := asSlice(m["users"])
	if len(users) == 0 {
		users = asSlice(m["contacts"])
	}
	for _, raw := range users {
		u := asMap(raw)
		id, ok := toInt64(u["id"])
		if !ok {
			continue
		}
		name := firstMaxName(u["names"])
		if name == "" {
			name = asString(u["title"])
		}
		if name != "" {
			a.cache.setName(id, name)
		}
	}

	chats := make([]maxChat, 0, 16)
	for _, raw := range asSlice(m["chats"]) {
		c := asMap(raw)
		id, ok := toInt64(c["id"])
		if !ok {
			continue
		}
		info := maxChat{
			id:    id,
			typ:   asString(c["type"]),
			title: asString(c["title"]),
		}
		a.cache.setChat(id, chatInfo{title: info.title, typ: info.typ})
		chats = append(chats, info)
	}
	return myID, chats
}

func maxErrorCode(v any) string {
	s := truncateText(asString(asMap(v)["error"]), 120)
	if s == "" {
		return "unknown"
	}
	return s
}

func (a *app) maxContact(chatID, sender int64) string {
	name, ok := a.cache.getName(sender)
	if !ok {
		name = fmt.Sprintf("ID %d", sender)
	}
	ci, ok := a.cache.getChat(chatID)
	if ok && ci.typ == "DIALOG" && ci.title != "" {
		return ci.title
	}
	if ok && ci.typ != "" && ci.title != "" {
		return name + " (Чат: " + ci.title + ")"
	}
	return name
}

func (a *app) handleMaxFrame(ws *wsConn, f maxFrame, myID int64, incomingOpcode int) {
	if f.cmd == 0 && f.opcode == maxOpcodePing {
		if ws.pingReplyAllowed() {
			_ = sendMaxFrame(ws, 0, 0, maxOpcodePing, map[string]any{"interactive": true})
		}
		return
	}
	if f.cmd == 3 {
		logf("[MAX] server error opcode %d: %s", f.opcode, safeLogText(maxErrorCode(f.payload)))
		return
	}
	if f.cmd != 0 || int(f.opcode) != incomingOpcode {
		return
	}

	p := asMap(f.payload)
	msg := asMap(p["message"])
	if len(msg) == 0 {
		return
	}
	sender, _ := toInt64(msg["sender"])
	if sender == 0 {
		sender, _ = toInt64(msg["userId"])
	}
	if sender == 0 || sender == myID {
		return
	}
	if strings.EqualFold(asString(msg["type"]), "SYSTEM") {
		return
	}
	chatID, _ := toInt64(p["chatId"])
	chatType := ""
	if ci, ok := a.cache.getChat(chatID); ok {
		chatType = ci.typ
		if ci.typ != "" && ci.typ != "DIALOG" {
			return // private chats only
		}
	}

	msgID, _ := toInt64(msg["id"])
	tm, _ := toInt64(msg["time"])
	cid, _ := toInt64(msg["cid"])
	key := fmt.Sprintf("max:%d/%d/%d/%d/%d", chatID, sender, msgID, tm, cid)
	text := asString(msg["text"])
	fresh := a.cache.markSeen(key)
	trace := nextTrace()
	debugf("[MAX] message trace=%d opcode=%d type=%s chatType=%s textLen=%d new=%t",
		trace, int(f.opcode), safeLogText(asString(msg["type"])), safeLogText(chatType), len(text), fresh)
	if !fresh {
		return
	}

	contact := a.maxContact(chatID, sender)
	a.tg.send("MAX", contact, text, trace)
	a.bark.send("MAX", contact, text, trace)
}

func (a *app) maxSession(ctx context.Context) error {
	cfg := a.configSnapshot()
	start := time.Now()
	ws, err := dialWebSocket(cfg.MaxWSURL, cfg.MaxUserAgent, "https://web.max.ru", 15*time.Second)
	if err != nil {
		debugf("[MAX] dial fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return err
	}
	debugf("[MAX] dial ok dur=%s", time.Since(start))
	defer ws.close()

	// Make "Выключить" interrupt a blocking WebSocket read immediately.
	// Closing the raw connection skips the close-frame write, which could
	// otherwise block for up to 10 seconds on a dead network.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.conn.Close()
		case <-done:
		}
	}()

	ua := map[string]any{
		"deviceType":      "WEB",
		"pushDeviceType":  "WEBPUSH",
		"locale":          "ru",
		"deviceLocale":    "ru",
		"osVersion":       "Linux",
		"deviceName":      "Chrome",
		"headerUserAgent": cfg.MaxUserAgent,
		"appVersion":      cfg.MaxAppVersion,
		"screen":          "1920x1080 1.0x",
		"timezone":        "Europe/Moscow",
	}

	var seq int16 = 1
	req := func(opcode int16, payload map[string]any) (int16, error) {
		s := seq
		seq++
		if seq >= 0x7fff {
			seq = 1
		}
		return s, sendMaxFrame(ws, 0, s, opcode, payload)
	}

	s, err := req(maxOpcodeInit, map[string]any{
		"deviceId":  cfg.MaxDeviceID,
		"userAgent": ua,
	})
	if err != nil {
		return err
	}
	if _, err := a.maxWait(ws, s, maxOpcodeInit); err != nil {
		return err
	}
	debugf("[MAX] init ok seq=%d", s)

	s, err = req(maxOpcodeLogin, map[string]any{
		"token":        cfg.MaxToken,
		"interactive":  true,
		"chatsCount":   40,
		"chatsSync":    40,
		"contactsSync": 0,
		"presenceSync": 0,
		"draftsSync":   0,
	})
	if err != nil {
		return err
	}
	login, err := a.maxWait(ws, s, maxOpcodeLogin)
	if err != nil {
		return err
	}
	debugf("[MAX] login ok seq=%d", s)

	myID, chats := a.parseMaxLogin(login.payload)
	subscribed := 0
	for _, ch := range chats {
		if ch.typ == "DIALOG" && subscribed < 50 {
			if _, err := req(maxOpcodeSub, map[string]any{"chatId": ch.id, "subscribe": true}); err != nil {
				return err
			}
			subscribed++
		}
	}
	debugf("[MAX] subscribed dialogs=%d", subscribed)
	logf("[MAX] websocket connected")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.setState("max", "on", "websocket подключён")

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := sendMaxFrame(ws, 0, 0, maxOpcodePing, map[string]any{"interactive": true}); err != nil {
					debugf("[MAX] keepalive ping fail err=%s", safeErrText(err))
					// Force the reader out of its blocking call so the
					// session reconnects instead of sitting on a dead socket.
					_ = ws.conn.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()

	bad := 0
	for {
		data, err := ws.readMessageTimeout(50 * time.Second)
		if err != nil {
			debugf("[MAX] read fail err=%s", safeErrText(err))
			return err
		}
		f, err := parseMaxFrame(data)
		if err != nil {
			bad++
			debugf("[MAX] bad frame count=%d err=%s", bad, safeErrText(err))
			if bad > 32 {
				return err
			}
			continue
		}
		bad = 0
		a.handleMaxFrame(ws, f, myID, cfg.IncomingOpcode)
	}
}

func (a *app) runMAX(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		err := a.maxSession(ctx)
		if ctx.Err() != nil {
			return
		}
		logf("[MAX] session error: %s", errKind(err))
		debugf("[MAX] session end attempt=%d err=%s", fails+1, safeErrText(err))
		a.stateErr("max", err)
		fails++
		if !sleepBackoff(ctx, fails) {
			return
		}
	}
}

func randomDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type maxRequester func(opcode int16, payload map[string]any) (int16, error)

// maxTempSession opens a short-lived connection, performs INIT and hands
// control to fn. It is used by the UI for validation and SMS auth only.
func maxTempSession(wsURL, deviceID, ua, appVersion string, fn func(*wsConn, maxRequester) error) error {
	if wsURL == "" {
		wsURL = "wss://api.oneme.ru/websocket"
	}
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	}
	if appVersion == "" {
		appVersion = "26.11.9"
	}
	if deviceID == "" {
		deviceID = randomDeviceID()
	}

	ws, err := dialWebSocket(wsURL, ua, "https://web.max.ru", 15*time.Second)
	if err != nil {
		return err
	}
	defer ws.close()

	var seq int16 = 1
	req := func(opcode int16, payload map[string]any) (int16, error) {
		s := seq
		seq++
		if seq >= 0x7fff {
			seq = 1
		}
		return s, sendMaxFrame(ws, 0, s, opcode, payload)
	}

	uaPayload := map[string]any{
		"deviceType":      "WEB",
		"pushDeviceType":  "WEBPUSH",
		"locale":          "ru",
		"deviceLocale":    "ru",
		"osVersion":       "Linux",
		"deviceName":      "Chrome",
		"headerUserAgent": ua,
		"appVersion":      appVersion,
		"screen":          "1920x1080 1.0x",
		"timezone":        "Europe/Moscow",
	}
	s, err := req(maxOpcodeInit, map[string]any{"deviceId": deviceID, "userAgent": uaPayload})
	if err != nil {
		return err
	}
	if _, err := waitMaxAck(ws, s, maxOpcodeInit); err != nil {
		return err
	}
	return fn(ws, req)
}

func maxValidate(token, deviceID, ua, appVersion, wsURL string) error {
	if token == "" {
		return fmt.Errorf("empty token")
	}
	return maxTempSession(wsURL, deviceID, ua, appVersion, func(ws *wsConn, req maxRequester) error {
		s, err := req(maxOpcodeLogin, map[string]any{
			"token":        token,
			"interactive":  true,
			"chatsCount":   1,
			"chatsSync":    0,
			"contactsSync": 0,
			"presenceSync": 0,
			"draftsSync":   0,
		})
		if err != nil {
			return err
		}
		_, err = waitMaxAck(ws, s, maxOpcodeLogin)
		return err
	})
}

func maxCaptchaLink(phone, deviceID, ua, appVersion, wsURL string) (string, error) {
	var link string
	err := maxTempSession(wsURL, deviceID, ua, appVersion, func(ws *wsConn, req maxRequester) error {
		s, err := req(maxOpcodeCaptcha, map[string]any{"source": "auth", "identifier": phone})
		if err != nil {
			return err
		}
		f, err := waitMaxAck(ws, s, maxOpcodeCaptcha)
		if err != nil {
			return err
		}
		link = asString(nested(f.payload, "link"))
		if link == "" {
			return fmt.Errorf("empty captcha link")
		}
		u, err := url.Parse(link)
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "https" && strings.ToLower(u.Scheme) != "http") {
			link = ""
			return fmt.Errorf("неверная ссылка капчи")
		}
		return nil
	})
	return link, err
}

func maxSMSStart(phone, captchaToken, deviceID, ua, appVersion, wsURL string) (string, error) {
	var verifyToken string
	err := maxTempSession(wsURL, deviceID, ua, appVersion, func(ws *wsConn, req maxRequester) error {
		payload := map[string]any{
			"phone":    phone,
			"type":     "START_AUTH",
			"language": "ru",
		}
		if captchaToken != "" {
			payload["captchaToken"] = captchaToken
		}
		s, err := req(maxOpcodeVerify, payload)
		if err != nil {
			return err
		}
		f, err := waitMaxAck(ws, s, maxOpcodeVerify)
		if err != nil {
			return err
		}
		verifyToken = asString(nested(f.payload, "token"))
		if verifyToken == "" {
			return fmt.Errorf("empty verification token")
		}
		return nil
	})
	return verifyToken, err
}

func maxSMSFinish(verifyToken, code, deviceID, ua, appVersion, wsURL string) (string, error) {
	var accessToken string
	err := maxTempSession(wsURL, deviceID, ua, appVersion, func(ws *wsConn, req maxRequester) error {
		s, err := req(maxOpcodeCode, map[string]any{
			"token":         verifyToken,
			"verifyCode":    code,
			"authTokenType": "CHECK_CODE",
		})
		if err != nil {
			return err
		}
		f, err := waitMaxAck(ws, s, maxOpcodeCode)
		if err != nil {
			return err
		}
		accessToken = asString(nested(f.payload, "tokenAttrs", "LOGIN", "token"))
		if accessToken == "" {
			return fmt.Errorf("empty access token")
		}
		return nil
	})
	return accessToken, err
}
