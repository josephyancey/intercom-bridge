package t5

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func announceServer(t *testing.T, a *Announcements, got chan<- Announcement) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, ok := a.Receive(w, r); ok {
			got <- m
		}
	}))
	t.Cleanup(srv.Close)
	h, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestAnnouncementRoundTrip(t *testing.T) {
	got := make(chan Announcement, 1)
	host, port := announceServer(t, NewAnnouncements("word"), got)
	voice := []byte{1, 2, 3, 4, 5, 6}
	if err := NewAnnouncements("word").Send(context.Background(), host, port, Announcement{From: "Rosalie's Echo", Voice: voice}); err != nil {
		t.Fatal(err)
	}
	m := <-got
	if m.From != "Rosalie's Echo" || string(m.Voice) != string(voice) {
		t.Fatalf("received %+v", m)
	}
}

func TestAnnouncementWrongWord(t *testing.T) {
	host, port := announceServer(t, NewAnnouncements("word"), make(chan Announcement, 1))
	err := NewAnnouncements("other").Send(context.Background(), host, port, Announcement{From: "x", Voice: []byte{1, 2}})
	if err == nil {
		t.Fatal("an announcement with another house word was taken")
	}
}

func TestAnnouncementClockSkew(t *testing.T) {
	got := make(chan Announcement, 1)
	recv := NewAnnouncements("word")
	recv.now = func() time.Time { return time.Now().Add(3 * time.Hour) } // the device's clock is ahead
	host, port := announceServer(t, recv, got)
	if err := NewAnnouncements("word").Send(context.Background(), host, port, Announcement{From: "x", Voice: []byte{1, 2}}); err != nil {
		t.Fatalf("not re-signed by the device's clock: %v", err)
	}
	<-got
}

func TestAnnouncementReplayRefused(t *testing.T) {
	a := NewAnnouncements("word")
	headers := map[string]string{fromHeader: "x", textHeader: "", "Content-Type": audioType}
	body := []byte{1, 2}
	auth := signAt("word", http.MethodPost, AnnouncePath, headers, body, time.Now())
	post := func() int {
		r := httptest.NewRequest(http.MethodPost, AnnouncePath, bytesReader(body))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		r.Header.Set(authHeader, auth)
		w := httptest.NewRecorder()
		a.Receive(w, r)
		return w.Code
	}
	if c := post(); c != http.StatusNoContent {
		t.Fatalf("first delivery got %d", c)
	}
	if c := post(); c != http.StatusForbidden {
		t.Fatalf("a replay got %d", c)
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
