package t5

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func serve(t *testing.T, key []byte, got chan<- *Link, names chan<- string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l, name, err := Accept(w, r, key)
		if err != nil {
			return
		}
		names <- name
		got <- l
		<-l.Gone
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

func TestCallAndTalk(t *testing.T) {
	key := Key("word")
	got, names := make(chan *Link, 1), make(chan string, 1)
	host, port := serve(t, key, got, names)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caller, err := Dial(ctx, host, port, key, "Kitchen")
	if err != nil {
		t.Fatal(err)
	}
	defer caller.Close()
	if n := <-names; n != "Kitchen" {
		t.Fatalf("callee heard %q", n)
	}
	callee := <-got
	_ = callee.Say(MsgRinging)
	_ = callee.Say(MsgAnswer)
	for _, want := range []byte{MsgRinging, MsgAnswer} {
		select {
		case m := <-caller.Control:
			if m != want {
				t.Fatalf("got %q, want %q", m, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no control message")
		}
	}
	frame := make([]byte, FrameBytes)
	frame[0] = 7
	if err := caller.SendAudio(frame); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-callee.Audio:
		if len(b) != FrameBytes || b[0] != 7 {
			t.Fatalf("audio arrived as %d bytes starting %d", len(b), b[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audio")
	}
	caller.Close()
	select {
	case <-callee.Gone:
	case <-time.After(2 * time.Second):
		t.Fatal("the callee did not see the hangup")
	}
}

func TestWrongWord(t *testing.T) {
	got, names := make(chan *Link, 1), make(chan string, 1)
	host, port := serve(t, Key("right"), got, names)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, host, port, Key("wrong"), "Kitchen"); err == nil {
		t.Fatal("a call with the wrong house word got through")
	}
}

func TestShownName(t *testing.T) {
	if got := ShownName("  Kitchen\x00 Portal "); got != "Kitchen Portal" {
		t.Fatalf("ShownName = %q", got)
	}
	if got := ShownName(""); got != "Another room" {
		t.Fatalf("ShownName of nothing = %q", got)
	}
}
