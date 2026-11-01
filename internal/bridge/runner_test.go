package bridge

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRunnerTakesTurns(t *testing.T) {
	r := NewRunner(Config{
		IntercomKey: testKey, HouseWord: testWord, Advertise: "127.0.0.1", Listen: "127.0.0.1",
		PortLow: 47200, PortHigh: 47299, BrowseFor: 50 * time.Millisecond, ForgetAfter: time.Hour,
		MaxCall: time.Minute, Version: "test", NoAdvertise: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), "bbbbbbbbbbbbbbbb", 0)
	t.Cleanup(r.standDown)
	var others []other
	r.others = func(context.Context) []other { return others }
	ctx := context.Background()

	others = []other{{id: "aaaaaaaaaaaaaaaa", address: "10.0.0.9", since: 1}}
	r.round(ctx)
	if r.Active() {
		t.Fatal("ran while another bridge runs")
	}
	if s := r.Status(); s.Active || s.StandingBy != "10.0.0.9" {
		t.Fatalf("status %+v, want standing by 10.0.0.9", s)
	}

	others = nil
	r.round(ctx)
	if !r.Active() {
		t.Fatal("did not run once the other bridge was gone")
	}

	others = []other{{id: "cccccccccccccccc", address: "10.0.0.8", since: time.Now().Add(time.Hour).UnixMilli()}}
	r.round(ctx)
	if !r.Active() {
		t.Fatal("stood down for a bridge that started later")
	}

	others = []other{{id: "cccccccccccccccc", address: "10.0.0.8", since: 1}}
	r.round(ctx)
	if r.Active() {
		t.Fatal("kept running beside a bridge that started first")
	}
}
