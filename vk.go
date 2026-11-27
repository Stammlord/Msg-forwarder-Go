package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type vkLongPoll struct {
	Key    string
	Server string
	Ts     int64
}

type vkUpdates struct {
	Ts      int64
	Failed  int
	Updates [][]any
}

func vkValidate(token, version string, client *http.Client) error {
	if token == "" {
		return fmt.Errorf("empty token")
	}
	if version == "" {
		version = "5.131"
	}
	q := url.Values{}
	q.Set("access_token", token)
	q.Set("v", version)
	q.Set("fields", "first_name")
	u := "https://api.vk.com/method/users.get?" + q.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return fmt.Errorf("%s", errKind(err))
	}
	defer drainResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	var out struct {
		Response []struct {
			ID int64 `json:"id"`
		} `json:"response"`
		Error *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return fmt.Errorf("bad response")
	}
	if out.Error != nil {
		return fmt.Errorf("api error %d", out.Error.Code)
	}
	if len(out.Response) == 0 {
		return fmt.Errorf("empty response")
	}
	return nil
}

func (a *app) vkGetLongPoll(ctx context.Context) (vkLongPoll, error) {
	var lp vkLongPoll
	cfg := a.configSnapshot()

	q := url.Values{}
	q.Set("access_token", cfg.VKToken)
	q.Set("v", cfg.VKAPIVersion)
	q.Set("lp_version", "3")
	q.Set("need_pts", "0")
	u := "https://api.vk.com/method/messages.getLongPollServer?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return lp, err
	}
	start := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		debugf("[VK] getLongPoll fail dur=%s err=%s", time.Since(start), safeErrText(err))
		return lp, err
	}
	defer drainResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return lp, fmt.Errorf("http status %d", resp.StatusCode)
	}

	var out struct {
		Response *struct {
			Key    string `json:"key"`
			Server string `json:"server"`
			Ts     int64  `json:"ts"`
		} `json:"response"`
		Error *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&out); err != nil {
		debugf("[VK] getLongPoll bad response status=%d dur=%s err=%s", resp.StatusCode, time.Since(start), safeErrText(err))
		return lp, err
	}
	if out.Error != nil {
		debugf("[VK] getLongPoll api error=%d status=%d dur=%s", out.Error.Code, resp.StatusCode, time.Since(start))
		return lp, fmt.Errorf("vk api error %d", out.Error.Code)
	}
	if out.Response == nil || out.Response.Key == "" || out.Response.Server == "" {
		debugf("[VK] getLongPoll empty response status=%d dur=%s", resp.StatusCode, time.Since(start))
		return lp, fmt.Errorf("empty long poll response")
	}
	debugf("[VK] getLongPoll ok ts=%d status=%d dur=%s", out.Response.Ts, resp.StatusCode, time.Since(start))
	lp.Key = out.Response.Key
	lp.Server = out.Response.Server
	lp.Ts = out.Response.Ts
	return lp, nil
}

func (a *app) vkPoll(ctx context.Context, lp vkLongPoll, ts int64) (vkUpdates, error) {
	var out vkUpdates

	base := lp.Server
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return out, err
	}
	host := strings.ToLower(u.Hostname())
	if strings.ToLower(u.Scheme) != "https" || u.User != nil ||
		(host != "vk.com" && !strings.HasSuffix(host, ".vk.com")) {
		return out, fmt.Errorf("vk: unsafe long poll server")
	}
	q := u.Query()
	q.Set("act", "a_check")
	q.Set("key", lp.Key)
	q.Set("ts", strconv.FormatInt(ts, 10))
	q.Set("wait", "25")
	q.Set("mode", "2")
	q.Set("version", "3")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, err
	}
	start := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		debugf("[VK] poll fail ts=%d dur=%s err=%s", ts, time.Since(start), safeErrText(err))
		return out, err
	}
	defer drainResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("http status %d", resp.StatusCode)
	}

	var raw struct {
		Ts      int64               `json:"ts"`
		Failed  int                 `json:"failed"`
		Updates [][]json.RawMessage `json:"updates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&raw); err != nil {
		debugf("[VK] poll bad response ts=%d status=%d dur=%s err=%s", ts, resp.StatusCode, time.Since(start), safeErrText(err))
		return out, err
	}
	debugf("[VK] poll ts=%d status=%d dur=%s updates=%d failed=%d nextTs=%d", ts, resp.StatusCode, time.Since(start), len(raw.Updates), raw.Failed, raw.Ts)
	out.Ts = raw.Ts
	out.Failed = raw.Failed
	out.Updates = make([][]any, 0, len(raw.Updates))
	for _, ev := range raw.Updates {
		row := make([]any, 0, len(ev))
		for _, item := range ev {
			var v any
			if json.Unmarshal(item, &v) == nil {
				row = append(row, v)
			} else {
				row = append(row, nil)
			}
		}
		out.Updates = append(out.Updates, row)
	}
	return out, nil
}

// maxVKRefreshes bounds consecutive failed=1 responses. Without it a server
// that keeps returning a fresh ts would make the poller spin without sleep.
const maxVKRefreshes = 3

func (a *app) runVK(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		lp, err := a.vkGetLongPoll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logf("[VK] get server failed: %s", errKind(err))
			debugf("[VK] reconnect attempt=%d err=%s", fails+1, safeErrText(err))
			a.stateErr("vk", err)
			fails++
			if !sleepBackoff(ctx, fails) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		logf("[VK] long poll connected")
		a.setState("vk", "on", "long poll подключён")
		fails = 0
		a.vkPollLoop(ctx, lp, a.vkPoll)
		fails++
		debugf("[VK] poll loop ended, reconnect attempt=%d", fails)
		if !sleepBackoff(ctx, fails) {
			return
		}
	}
}

// vkPollLoop is split out of runVK so the failed=1 guard can be tested
// without a live VK server. The poller is injected for the same reason.
func (a *app) vkPollLoop(ctx context.Context, lp vkLongPoll, poll func(context.Context, vkLongPoll, int64) (vkUpdates, error)) {
	ts := lp.Ts
	refreshes := 0
	for ctx.Err() == nil {
		upd, err := poll(ctx, lp, ts)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logf("[VK] poll failed: %s", errKind(err))
			debugf("[VK] poll loop stop ts=%d err=%s", ts, safeErrText(err))
			a.stateErr("vk", err)
			return
		}
		if upd.Failed != 0 {
			// failed=1 is normal key expiry: the response carries a fresh ts
			// and the same key can be reused. Bound the retries so a broken
			// server cannot create a hot loop.
			if upd.Failed == 1 && upd.Ts != 0 && refreshes < maxVKRefreshes {
				refreshes++
				logf("[VK] long poll ts refreshed (failed=1)")
				debugf("[VK] ts refresh %d -> %d (failed=1)", ts, upd.Ts)
				ts = upd.Ts
				continue
			}
			debugf("[VK] key expired failed=%d ts=%d", upd.Failed, ts)
			logf("[VK] long poll key expired (failed=%d)", upd.Failed)
			a.setState("vk", "error", fmt.Sprintf("key expired (failed=%d)", upd.Failed))
			return
		}
		refreshes = 0
		if upd.Ts != 0 {
			ts = upd.Ts
		}
		for _, ev := range upd.Updates {
			a.handleVKEvent(ctx, ev)
		}
	}
}

// sleepBackoff returns false when the service was stopped during the wait.
func sleepBackoff(ctx context.Context, attempt int) bool {
	d := backoff(attempt)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	d = backoffJitter(d)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// vkIncoming extracts private incoming messages. VK flags: 2 = outgoing,
// 16 = chat, 128 = deleted. (8 = important, not a chat marker!)
func vkIncoming(ev []any) (peer, msgID int64, text string, ok bool) {
	if len(ev) < 6 {
		return 0, 0, "", false
	}
	code, has := toInt64(ev[0])
	if !has || code != 4 {
		return 0, 0, "", false
	}
	flags, has := toInt64(ev[2])
	if !has || flags&2 != 0 || flags&16 != 0 || flags&128 != 0 {
		return 0, 0, "", false
	}
	peer, has = toInt64(ev[3])
	if !has || peer <= 0 {
		return 0, 0, "", false
	}
	msgID, _ = toInt64(ev[1])
	return peer, msgID, asString(ev[5]), true
}

func (a *app) handleVKEvent(ctx context.Context, ev []any) {
	peer, msgID, text, ok := vkIncoming(ev)
	if !ok {
		return
	}
	// Message IDs are unique per peer, not globally: include the peer.
	key := fmt.Sprintf("vk:%d:%d", peer, msgID)
	fresh := a.cache.markSeen(key)
	trace := nextTrace()
	debugf("[VK] event trace=%d peer#%s msg#%s fields=%d textLen=%d new=%t",
		trace, maskID(strconv.FormatInt(peer, 10)), maskID(strconv.FormatInt(msgID, 10)), len(ev), len(text), fresh)
	if !fresh {
		return
	}
	contact := a.vkName(ctx, peer)
	a.tg.send("VK", contact, text, trace)
	a.bark.send("VK", contact, text, trace)
}

func (a *app) vkName(ctx context.Context, id int64) string {
	if name, ok := a.cache.getName(id); ok {
		debugf("[VK] name cache hit id#%s", maskID(strconv.FormatInt(id, 10)))
		return name
	}
	name := fmt.Sprintf("ID %d", id)
	if ctx.Err() != nil {
		return name
	}
	cfg := a.configSnapshot()

	q := url.Values{}
	q.Set("user_ids", strconv.FormatInt(id, 10))
	q.Set("fields", "first_name,last_name")
	q.Set("access_token", cfg.VKToken)
	q.Set("v", cfg.VKAPIVersion)
	u := "https://api.vk.com/method/users.get?" + q.Encode()

	// A slow name lookup must not stall long polling (and therefore message
	// forwarding); stopping the worker cancels the lookup as well.
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, u, nil)
	if err != nil {
		return name
	}
	start := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		logf("[VK] name lookup failed: %s", errKind(err))
		debugf("[VK] name lookup fail id#%s dur=%s err=%s", maskID(strconv.FormatInt(id, 10)), time.Since(start), safeErrText(err))
		// Do not cache the numeric fallback on a transport error: the next
		// message should retry the lookup.
		return name
	}
	defer drainResponse(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return name
	}

	var out struct {
		Response []struct {
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"response"`
		Error *struct {
			Code int `json:"error_code"`
		} `json:"error"`
	}
	resolved := false
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) == nil && out.Error == nil && len(out.Response) > 0 {
		full := strings.TrimSpace(out.Response[0].FirstName + " " + out.Response[0].LastName)
		if full != "" {
			name = full
			resolved = true
		}
	}
	debugf("[VK] name lookup id#%s resolved=%t dur=%s", maskID(strconv.FormatInt(id, 10)), resolved, time.Since(start))
	a.cache.setName(id, name)
	return name
}
