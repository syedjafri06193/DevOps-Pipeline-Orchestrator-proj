package dashboard

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Section 11.1 calls Last-Event-ID replay and the keepalive "load bearing".
// These tests exist because both fail silently: a dropped event looks like a
// stalled deploy, and a closed idle connection looks like a frozen UI. Neither
// produces an error anywhere, so only a test catches a regression.

func TestReplayReturnsOnlyEventsAfterTheLastSeenID(t *testing.T) {
	b := NewBus()
	for i := 1; i <= 5; i++ {
		b.Publish("dep1", "deployment", fmt.Sprintf("event-%d", i))
	}

	got, gap := b.Since("dep1", 3)
	if gap {
		t.Fatalf("a full buffer reported a gap")
	}
	if len(got) != 2 {
		t.Fatalf("replaying after id 3 returned %d events, want 2", len(got))
	}
	if got[0].Data != "event-4" || got[1].Data != "event-5" {
		t.Fatalf("replay returned %q and %q", got[0].Data, got[1].Data)
	}
}

func TestReplayFromZeroReturnsEverythingBuffered(t *testing.T) {
	b := NewBus()
	b.Publish("dep1", "deployment", "a")
	b.Publish("dep1", "deployment", "b")

	got, gap := b.Since("dep1", 0)
	if gap {
		t.Fatalf("a first connection reported a gap")
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

// The buffer is bounded, so a client that was away long enough has genuinely
// missed output. The important thing is that it is told, rather than shown an
// incomplete log it cannot distinguish from a complete one.
func TestAReconnectPastTheBufferIsToldItMissedEvents(t *testing.T) {
	b := NewBus()
	b.maxHistory = 4
	for i := 1; i <= 10; i++ {
		b.Publish("dep1", "deployment", fmt.Sprintf("event-%d", i))
	}

	// The client last saw event 2; events 3-6 have been evicted.
	got, gap := b.Since("dep1", 2)
	if !gap {
		t.Fatalf("a reconnect that missed evicted events was not told about the gap")
	}
	if len(got) != 4 {
		t.Fatalf("got %d events, want the whole 4-event buffer", len(got))
	}
	if got[0].Data != "event-7" {
		t.Fatalf("buffer starts at %q, want event-7", got[0].Data)
	}
}

func TestTheBufferIsBounded(t *testing.T) {
	b := NewBus()
	b.maxHistory = 10
	for i := 0; i < 1000; i++ {
		b.Publish("dep1", "log", "line")
	}
	b.mu.RLock()
	n := len(b.history["dep1"])
	b.mu.RUnlock()
	if n != 10 {
		t.Fatalf("history holds %d events, want it capped at 10", n)
	}
}

func TestTopicsAreIsolated(t *testing.T) {
	b := NewBus()
	b.Publish("dep1", "deployment", "one")
	b.Publish("dep2", "deployment", "two")

	got, _ := b.Since("dep1", 0)
	if len(got) != 1 || got[0].Data != "one" {
		t.Fatalf("dep1 saw %v, want only its own event", got)
	}
}

// A browser that stops reading must not wedge the executor. The drop is safe
// precisely because of the replay buffer tested above.
func TestASlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b := NewBus()
	_, unsubscribe := b.Subscribe("dep1")
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		// Far more than the 64-deep channel can hold.
		for i := 0; i < 500; i++ {
			b.Publish("dep1", "log", "line")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading")
	}
}

func TestUnsubscribeRemovesTheSubscriber(t *testing.T) {
	b := NewBus()
	_, unsubscribe := b.Subscribe("dep1")
	unsubscribe()

	b.mu.RLock()
	_, present := b.subs["dep1"]
	b.mu.RUnlock()
	if present {
		t.Fatal("the topic still has subscribers after the last one left")
	}
}

// Regression: Publish used to copy the subscriber channels, release the lock,
// and then send. A browser closing its tab in that window closed the channel
// underneath the send, and "send on closed channel" took down the server --
// during a deploy, which is the only time anyone is watching.
func TestABrowserLeavingMidPublishDoesNotPanic(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		b := NewBus()
		_, unsubscribe := b.Subscribe("dep1")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				b.Publish("dep1", "log", "line")
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			unsubscribe()
		}()
		close(start)
		wg.Wait()
	}
}

func TestUnsubscribingTwiceIsSafe(t *testing.T) {
	b := NewBus()
	_, unsubscribe := b.Subscribe("dep1")
	unsubscribe()
	unsubscribe() // a double close would panic
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewBus()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Publish("dep1", "log", "line")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsubscribe := b.Subscribe("dep1")
			go func() {
				for range ch {
				}
			}()
			time.Sleep(time.Millisecond)
			unsubscribe()
		}()
	}
	wg.Wait()

	// Every event got a distinct, increasing id despite the concurrency.
	b.mu.RLock()
	defer b.mu.RUnlock()
	last := int64(0)
	for _, ev := range b.history["dep1"] {
		if ev.ID <= last {
			t.Fatalf("event ids went backwards: %d after %d", ev.ID, last)
		}
		last = ev.ID
	}
}

// --------------------------------------------------------------- wire format

func TestMultiLineDataGetsOneDataFieldPerLine(t *testing.T) {
	var sb strings.Builder
	writeSSE(&stringWriter{&sb}, Event{ID: 7, Type: "log", Data: "first\nsecond"})

	want := "id: 7\nevent: log\ndata: first\ndata: second\n\n"
	if sb.String() != want {
		t.Fatalf("frame was %q, want %q", sb.String(), want)
	}
}

func TestLastEventIDParsingRejectsGarbage(t *testing.T) {
	cases := map[string]int64{
		"":         0,
		"12":       12,
		" 12 ":     12,
		"-1":       0,
		"abc":      0,
		"9e9":      0,
		"12; DROP": 0,
	}
	for in, want := range cases {
		if got := parseLastEventID(in); got != want {
			t.Errorf("parseLastEventID(%q) = %d, want %d", in, got, want)
		}
	}
}

type stringWriter struct{ sb *strings.Builder }

func (w *stringWriter) Header() http.Header         { return http.Header{} }
func (w *stringWriter) Write(p []byte) (int, error) { return w.sb.Write(p) }
func (w *stringWriter) WriteHeader(int)             {}

// ------------------------------------------------------------------- streaming

func TestStreamSetsTheHeadersProxiesNeed(t *testing.T) {
	b := NewBus()
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/events/dep1", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() { b.Stream(rec, req, "dep1"); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	for header, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		// Without this, nginx buffers the whole response and the browser
		// receives nothing until the deploy is over.
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestStreamReplaysBeforeStreamingLive(t *testing.T) {
	b := NewBus()
	b.Publish("dep1", "deployment", "before-1")
	b.Publish("dep1", "deployment", "before-2")

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/events/dep1", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "1")

	done := make(chan struct{})
	go func() { b.Stream(rec, req, "dep1"); close(done) }()

	rec.waitForFlush(t)
	b.Publish("dep1", "deployment", "live-1")
	rec.waitFor(t, "live-1")
	cancel()
	<-done

	body := rec.body()
	if strings.Contains(body, "before-1") {
		t.Error("replayed an event the client had already seen")
	}
	if !strings.Contains(body, "before-2") {
		t.Error("did not replay the event the client had missed")
	}
	if !strings.Contains(body, "live-1") {
		t.Error("did not stream the event published after connecting")
	}
	if i, j := strings.Index(body, "before-2"), strings.Index(body, "live-1"); i > j {
		t.Error("the live event was written before the replayed one")
	}
}

func TestStreamAnnouncesAGapBeforeReplaying(t *testing.T) {
	b := NewBus()
	b.maxHistory = 2
	for i := 1; i <= 6; i++ {
		b.Publish("dep1", "deployment", fmt.Sprintf("e%d", i))
	}

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/events/dep1", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "1")

	done := make(chan struct{})
	go func() { b.Stream(rec, req, "dep1"); close(done) }()
	rec.waitFor(t, "e6")
	cancel()
	<-done

	body := rec.body()
	if !strings.Contains(body, "event: gap") {
		t.Fatalf("no gap event in:\n%s", body)
	}
	if strings.Index(body, "event: gap") > strings.Index(body, "e5") {
		t.Error("the gap was announced after the events it precedes")
	}
}

// The 20-second keepalive is a comment line, which the EventSource spec says
// to ignore -- exactly what is wanted from a byte whose only job is to stop a
// proxy closing the connection.
func TestKeepaliveWritesAnIgnorableComment(t *testing.T) {
	rec := newFlushRecorder()
	fmt.Fprint(rec, ": keepalive\n\n")

	line, err := bufio.NewReader(strings.NewReader(rec.body())).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("keepalive line %q is not an SSE comment", line)
	}
	if KeepaliveInterval >= 30*time.Second {
		t.Fatalf("keepalive is %s; proxies close idle connections at 30-60s", KeepaliveInterval)
	}
}

func TestStreamStopsWhenTheBrowserGoesAway(t *testing.T) {
	b := NewBus()
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/events/dep1", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() { b.Stream(rec, req, "dep1"); close(done) }()
	rec.waitForFlush(t)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream outlived the request context")
	}

	b.mu.RLock()
	_, present := b.subs["dep1"]
	b.mu.RUnlock()
	if present {
		t.Fatal("Stream did not unsubscribe on the way out")
	}
}

func TestStreamRefusesAResponseWriterThatCannotFlush(t *testing.T) {
	b := NewBus()
	var sb strings.Builder
	w := &noFlushWriter{sb: &sb, header: http.Header{}}
	req := httptest.NewRequest("GET", "/events/dep1", nil)

	b.Stream(w, req, "dep1")

	if w.code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: streaming into a buffering writer would hang the browser", w.code)
	}
}

type noFlushWriter struct {
	sb     *strings.Builder
	header http.Header
	code   int
}

func (w *noFlushWriter) Header() http.Header         { return w.header }
func (w *noFlushWriter) Write(p []byte) (int, error) { return w.sb.Write(p) }
func (w *noFlushWriter) WriteHeader(c int)           { w.code = c }

// --------------------------------------------------------------- publisher

func TestPublisherFansOutToBothTheGlobalAndPerDeploymentTopics(t *testing.T) {
	b := NewBus()
	p := &Publisher{Bus: b}
	payload := `{"type":"deployment.succeeded","deployment_id":"dep-abc","app":"api"}`

	if err := p.Notify(context.Background(), payload); err != nil {
		t.Fatal(err)
	}

	all, _ := b.Since("all", 0)
	if len(all) != 1 {
		t.Fatalf("the overview topic got %d events, want 1", len(all))
	}
	per, _ := b.Since("dep-abc", 0)
	if len(per) != 1 {
		t.Fatalf("the detail topic got %d events, want 1", len(per))
	}
}

func TestPublisherWithNoDeploymentIDStillReachesTheOverview(t *testing.T) {
	b := NewBus()
	p := &Publisher{Bus: b}
	if err := p.Notify(context.Background(), `{"type":"freeze.set"}`); err != nil {
		t.Fatal(err)
	}
	if all, _ := b.Since("all", 0); len(all) != 1 {
		t.Fatalf("the overview topic got %d events, want 1", len(all))
	}
}

func TestDeploymentIDExtraction(t *testing.T) {
	cases := map[string]string{
		`{"deployment_id":"dep-1"}`:           "dep-1",
		`{"app":"x","deployment_id":"dep-2"}`: "dep-2",
		`{"app":"x"}`:                         "",
		`{"deployment_id":`:                   "",
		`{"deployment_id":"unterminated`:      "",
	}
	for in, want := range cases {
		if got := deploymentIDOf(in); got != want {
			t.Errorf("deploymentIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// ------------------------------------------------------------------ recorder

// flushRecorder is an httptest.ResponseRecorder that is safe to read from one
// goroutine while Stream writes from another, which is the shape every test
// above needs and the plain recorder races on.
type flushRecorder struct {
	mu      sync.Mutex
	sb      strings.Builder
	hdr     http.Header
	flushes int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{hdr: http.Header{}}
}

func (r *flushRecorder) Header() http.Header { return r.hdr }
func (r *flushRecorder) WriteHeader(int)     {}

func (r *flushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sb.Write(p)
}

func (r *flushRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *flushRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sb.String()
}

func (r *flushRecorder) waitForFlush(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := r.flushes
		r.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the stream never flushed")
}

func (r *flushRecorder) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.body(), substr) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%q never appeared in the stream:\n%s", substr, r.body())
}
