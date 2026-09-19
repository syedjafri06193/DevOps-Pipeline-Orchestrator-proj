// Package dashboard is the embedded UI (design.md section 11).
//
//	"Server-rendered + SSE, not an SPA. The dashboard's primary job is showing
//	live deployment progress and streaming logs. That is a server-push
//	problem, and a React SPA with polling is more code for a worse result."
//
// SSE over WebSockets "because it's unidirectional (which is all you need), it
// reconnects automatically, it's plain HTTP, and it survives proxies that
// mangle WebSocket upgrades."
//
// Two details the document calls load bearing, and both are:
//
//   - `Last-Event-ID` replay. Without it a reconnect silently drops log lines,
//     and the operator watching a deploy sees a gap they cannot tell from a
//     stall.
//   - The 20-second keepalive. Without it, proxies close idle connections
//     after 30-60 seconds and the UI appears to freeze -- which during a
//     ten-minute bake is most of the time.
package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one server-sent event.
type Event struct {
	ID   int64
	Type string
	Data string
}

// Bus fans events out to connected browsers and keeps a replay buffer.
type Bus struct {
	mu   sync.RWMutex
	subs map[string]map[int]chan Event
	// history is the replay buffer, per topic. Bounded: a deploy that emits
	// ten thousand log lines must not hold them all in memory forever, and a
	// reconnect that missed more than this gets a gap it is told about rather
	// than one it cannot detect.
	history    map[string][]Event
	nextID     int64
	nextSub    int
	maxHistory int
}

func NewBus() *Bus {
	return &Bus{
		subs:       map[string]map[int]chan Event{},
		history:    map[string][]Event{},
		maxHistory: 500,
	}
}

// Publish sends an event to a topic and records it for replay.
//
// The fan-out happens under the same lock that guards the subscriber map,
// rather than against a copy taken and released first. Copying was the obvious
// shape and it is wrong: between releasing the lock and sending, an
// unsubscribing browser closes its channel, and the send panics the whole
// process with "send on closed channel". A browser closing a tab during a
// deploy is not an exceptional event, so the window is hit in normal use.
//
// Holding the lock is safe precisely because every send is non-blocking: the
// loop below cannot wait on anyone, so it cannot hold the lock for longer than
// it takes to walk a handful of channels.
func (b *Bus) Publish(topic, eventType, data string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	ev := Event{ID: b.nextID, Type: eventType, Data: data}

	h := append(b.history[topic], ev)
	if len(h) > b.maxHistory {
		h = h[len(h)-b.maxHistory:]
	}
	b.history[topic] = h

	for _, ch := range b.subs[topic] {
		select {
		case ch <- ev:
		default:
			// A slow browser must not block the executor. Dropping is safe
			// because the client reconnects with Last-Event-ID and replays
			// what it missed -- which is the reason the replay buffer exists.
		}
	}
}

// Subscribe returns a channel and an unsubscribe function.
func (b *Bus) Subscribe(topic string) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextSub++
	id := b.nextSub
	ch := make(chan Event, 64)
	if b.subs[topic] == nil {
		b.subs[topic] = map[int]chan Event{}
	}
	b.subs[topic][id] = ch

	// Idempotent: a handler that unsubscribes on both a normal return and a
	// deferred cleanup path would otherwise close the channel twice, and a
	// double close is a panic rather than a no-op.
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if m, ok := b.subs[topic]; ok {
				delete(m, id)
				if len(m) == 0 {
					delete(b.subs, topic)
				}
			}
			// Closed with the lock held, after the channel is out of the
			// subscriber map, so no in-flight Publish can still be holding it.
			close(ch)
		})
	}
}

// Since returns buffered events after an id, and whether any were lost.
//
// The second return value is what lets the client be told "you missed some"
// rather than silently showing an incomplete log, which is the failure the
// replay buffer exists to prevent and would otherwise reintroduce at the
// buffer's edge.
func (b *Bus) Since(topic string, lastID int64) ([]Event, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	h := b.history[topic]
	if len(h) == 0 {
		return nil, false
	}
	if lastID == 0 {
		return append([]Event(nil), h...), false
	}
	gap := h[0].ID > lastID+1
	var out []Event
	for _, ev := range h {
		if ev.ID > lastID {
			out = append(out, ev)
		}
	}
	return out, gap
}

// KeepaliveInterval is the document's 20 seconds: short enough to beat the
// 30-60 second idle timeout of every proxy in the path.
const KeepaliveInterval = 20 * time.Second

// Stream serves an SSE connection for one topic.
func (b *Bus) Stream(w http.ResponseWriter, r *http.Request, topic string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers responses by default, which for SSE means the browser
	// receives nothing until the connection closes.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsubscribe := b.Subscribe(topic)
	defer unsubscribe()

	// Replay from the caller's last seen event so reconnects do not lose
	// output. Subscribing before replaying, so an event published between
	// the two is delivered rather than falling into the gap.
	lastID := parseLastEventID(r.Header.Get("Last-Event-ID"))
	missed, gap := b.Since(topic, lastID)
	if gap {
		writeSSE(w, Event{Type: "gap", Data: `{"message":"some events were not buffered; reload for the full history"}`})
	}
	for _, ev := range missed {
		writeSSE(w, ev)
	}
	flusher.Flush()

	keepalive := time.NewTicker(KeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.ID <= lastID {
				continue // already replayed
			}
			lastID = ev.ID
			writeSSE(w, ev)
			flusher.Flush()
		case <-keepalive.C:
			// A comment line. Without it, proxies close the idle connection
			// and the UI appears to freeze.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev Event) {
	if ev.ID > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.ID)
	}
	if ev.Type != "" {
		fmt.Fprintf(w, "event: %s\n", ev.Type)
	}
	// Multi-line data needs one `data:` per line, or everything after the
	// first newline is silently dropped by the browser.
	for _, line := range strings.Split(ev.Data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

func parseLastEventID(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Publisher adapts the bus to the engine's notifier interface, so deployment
// events reach the browser through the same path as Slack.
type Publisher struct{ Bus *Bus }

func (p *Publisher) Name() string { return "dashboard" }

func (p *Publisher) Notify(ctx context.Context, payload string) error {
	p.Bus.Publish("all", "deployment", payload)
	// Also publish to the per-deployment topic, so a detail page does not
	// receive every other deployment's events.
	if id := deploymentIDOf(payload); id != "" {
		p.Bus.Publish(id, "deployment", payload)
	}
	return nil
}

func deploymentIDOf(payload string) string {
	const key = `"deployment_id":"`
	i := strings.Index(payload, key)
	if i < 0 {
		return ""
	}
	rest := payload[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
