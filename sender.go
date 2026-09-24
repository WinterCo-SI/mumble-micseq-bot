package main

import (
	"context"
	"html"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"layeh.com/gumble/gumble"
)

// maxQueuedAge drops messages that could not be sent in time (for example
// while disconnected); stale countdowns only confuse people.
const maxQueuedAge = 2 * time.Minute

type outMsg struct {
	toUser    bool
	channelID uint32
	session   uint32
	html      string
	coalesce  string
	queued    time.Time
	// conn is the connection a private message was addressed on. Session
	// IDs are reused after a server restart, so private messages are never
	// delivered on a different connection. Channel IDs are persistent.
	conn *gumble.Client
}

// Sender paces outgoing text messages. Murmur silently drops text messages
// above its messagelimit/messageburst, so everything goes through a token
// bucket, and a newer periodic message replaces an older unsent one.
type Sender struct {
	interval time.Duration
	burst    int

	mu    sync.Mutex
	queue []*outMsg
	wake  chan struct{}

	client    atomic.Pointer[gumble.Client]
	maxLen    atomic.Int64
	allowHTML atomic.Bool
}

func NewSender(rate float64, burst int) *Sender {
	s := &Sender{
		interval: time.Duration(float64(time.Second) / rate),
		burst:    burst,
		wake:     make(chan struct{}, 1),
	}
	s.maxLen.Store(5000)
	s.allowHTML.Store(true)
	return s
}

// SetClient sets the connection to send on; nil pauses sending.
func (s *Sender) SetClient(c *gumble.Client) {
	s.client.Store(c)
	s.signal()
}

func (s *Sender) SetServerConfig(e *gumble.ServerConfigEvent) {
	if e.MaximumMessageLength != nil && *e.MaximumMessageLength > 0 {
		s.maxLen.Store(int64(*e.MaximumMessageLength))
	}
	if e.AllowHTML != nil {
		s.allowHTML.Store(*e.AllowHTML)
	}
}

func (s *Sender) ToChannel(channelID uint32, text, coalesce string) {
	s.enqueue(&outMsg{channelID: channelID, html: text, coalesce: coalesce})
}

// ToUser queues a private message for a session on connection c.
func (s *Sender) ToUser(c *gumble.Client, session uint32, text string) {
	s.enqueue(&outMsg{toUser: true, session: session, html: text, conn: c})
}

func (s *Sender) enqueue(m *outMsg) {
	m.queued = time.Now()
	s.mu.Lock()
	replaced := false
	if m.coalesce != "" {
		for i, q := range s.queue {
			if q.coalesce == m.coalesce {
				s.queue[i] = m
				replaced = true
				break
			}
		}
	}
	if !replaced {
		s.queue = append(s.queue, m)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *Sender) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sender) pop() *outMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) > 0 {
		m := s.queue[0]
		s.queue = s.queue[1:]
		if time.Since(m.queued) <= maxQueuedAge {
			return m
		}
	}
	return nil
}

func (s *Sender) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) > 0
}

// Run sends queued messages until ctx is cancelled.
func (s *Sender) Run(ctx context.Context) {
	tokens := float64(s.burst)
	last := time.Now()
	for {
		now := time.Now()
		tokens += float64(now.Sub(last)) / float64(s.interval)
		if tokens > float64(s.burst) {
			tokens = float64(s.burst)
		}
		last = now

		c := s.client.Load()
		if c != nil && c.State() != gumble.StateSynced {
			c = nil // connection is going away; keep messages for the next one
		}
		if c != nil && tokens >= 1 && s.pending() {
			if m := s.pop(); m != nil {
				func() {
					defer logPanic("send message")
					s.send(c, m)
				}()
				tokens--
			}
			continue
		}

		var wait <-chan time.Time
		switch {
		case c != nil && s.pending():
			wait = time.After(time.Duration((1 - tokens) * float64(s.interval)))
		case s.client.Load() != nil && s.pending():
			wait = time.After(time.Second) // re-check the connection state
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-wait:
		}
	}
}

func (s *Sender) send(c *gumble.Client, m *outMsg) {
	if m.toUser && m.conn != c {
		return
	}
	text := s.render(m.html)
	doWithTimeout(c, doTimeout, func() {
		if m.toUser {
			if u := c.Users[m.session]; u != nil {
				u.Send(text)
			}
			return
		}
		if ch := c.Channels[m.channelID]; ch != nil {
			ch.Send(text, false)
		}
	})
}

var (
	brTag    = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTag   = regexp.MustCompile(`<[^>]*>`)
	ellipsis = "……"
)

func (s *Sender) render(text string) string {
	if !s.allowHTML.Load() {
		text = brTag.ReplaceAllString(text, "\n")
		text = html.UnescapeString(anyTag.ReplaceAllString(text, ""))
	}
	max := int(s.maxLen.Load())
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	// Cut on a line break so no HTML tag or entity is split.
	runes := []rune(text)
	cut := string(runes[:max-utf8.RuneCountInString(ellipsis)])
	if i := strings.LastIndex(cut, "<br>"); i > 0 {
		cut = cut[:i+len("<br>")]
	} else if i := strings.LastIndex(cut, "\n"); i > 0 {
		cut = cut[:i+1]
	}
	return cut + ellipsis
}
