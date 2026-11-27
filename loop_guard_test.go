package main

import (
	"context"
	"testing"
	"time"
)

func TestVKFailedRefreshIsBounded(t *testing.T) {
	a := &app{}
	initStates(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	poll := func(context.Context, vkLongPoll, int64) (vkUpdates, error) {
		calls++
		return vkUpdates{Ts: int64(100 + calls), Failed: 1}, nil
	}
	a.vkPollLoop(ctx, vkLongPoll{Server: "https://vk.com", Key: "k", Ts: 1}, poll)
	if calls != maxVKRefreshes+1 {
		t.Fatalf("polled %d times, want %d", calls, maxVKRefreshes+1)
	}
}

func TestVKRefreshCounterResetsOnSuccess(t *testing.T) {
	a := &app{}
	initStates(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	poll := func(context.Context, vkLongPoll, int64) (vkUpdates, error) {
		calls++
		upd := vkUpdates{Ts: int64(100 + calls)}
		if calls%2 == 1 {
			upd.Failed = 1
		}
		if calls == 5 {
			cancel()
		}
		return upd, nil
	}
	a.vkPollLoop(ctx, vkLongPoll{Server: "https://vk.com", Key: "k", Ts: 1}, poll)
	if calls != 5 {
		t.Fatalf("polled %d times, want 5", calls)
	}
}

func TestMAXPingReplyRateLimited(t *testing.T) {
	w := &wsConn{}
	if !w.pingReplyAllowed() {
		t.Fatal("first server ping must be answerable")
	}
	w.notePingSent()
	if w.pingReplyAllowed() {
		t.Fatal("ping echo was answered immediately")
	}
	w.lastPingSend = time.Now().Add(-maxPingReplyInterval - time.Second)
	if !w.pingReplyAllowed() {
		t.Fatal("ping older than the interval must be answerable")
	}
}

func TestBackoffJitter(t *testing.T) {
	if got := backoffJitter(0); got != 0 {
		t.Fatalf("zero jitter: %v", got)
	}
	base := 8 * time.Second
	for i := 0; i < 200; i++ {
		got := backoffJitter(base)
		if got < base || got > base+base/4 {
			t.Fatalf("jitter %v outside [%v, %v]", got, base, base+base/4)
		}
	}
}
