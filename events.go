package main

// Live-Events: Wertänderungen aus den Callbacks (callback.go) werden über den Hub
// per Server-Sent Events (/api/events) weitergereicht.

import (
	"sync"
	"time"
)

type Event struct {
	Interface string      `json:"interface"`
	Address   string      `json:"address"`
	Param     string      `json:"param"`
	Value     interface{} `json:"value"`
	TS        time.Time   `json:"ts"`
}

type Hub struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	last   map[string]Event
	closed bool
}

func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}, last: map[string]Event{}}
}

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 256)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.subs[ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[e.Address+"|"+e.Param] = e
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // langsamer Client: Event verwerfen
		}
	}
}

func (h *Hub) Last() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, 0, len(h.last))
	for _, e := range h.last {
		out = append(out, e)
	}
	return out
}

func (h *Hub) Count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.subs) }

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}
