package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"micseqbot/internal/micseq"

	"layeh.com/gumble/gumble"
	"layeh.com/gumble/gumbleutil"
)

// Bot connects the pure micseq.Manager to a Mumble server through gumble.
type Bot struct {
	cfg    *Config
	mgr    *micseq.Manager
	sender *Sender
	ignore map[string]bool

	persistMu sync.Mutex
	persist   chan struct{}
}

func NewBot(cfg *Config) *Bot {
	ignore := map[string]bool{}
	for _, n := range cfg.IgnoreUsers {
		ignore[strings.ToLower(n)] = true
	}
	return &Bot{
		cfg:     cfg,
		mgr:     micseq.NewManager(cfg.managerConfig()),
		sender:  NewSender(cfg.MessageRate, cfg.MessageBurst),
		ignore:  ignore,
		persist: make(chan struct{}, 1),
	}
}

// LoadState restores the saved queue state, if any.
func (b *Bot) LoadState() error {
	data, err := micseq.LoadStateFile(b.cfg.StateFile)
	if err != nil || data == nil {
		return err
	}
	if err := b.mgr.RestoreState(data); err != nil {
		return fmt.Errorf("%s: %w", b.cfg.StateFile, err)
	}
	log.Printf("restored state from %s", b.cfg.StateFile)
	return nil
}

// SaveState writes the current state to disk.
func (b *Bot) SaveState() {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()
	data, err := b.mgr.MarshalState()
	if err == nil {
		err = micseq.SaveStateFile(b.cfg.StateFile, data)
	}
	if err != nil {
		log.Printf("save state: %v", err)
	}
}

// Run keeps the bot connected until ctx is cancelled. Connection losses (for
// example a server restart) are retried forever with exponential backoff;
// the queue state lives in the manager and survives reconnects.
func (b *Bot) Run(ctx context.Context) {
	go b.sender.Run(ctx)
	go b.persistLoop(ctx)
	defer b.SaveState()

	backoff := minBackoff
	for {
		start := time.Now()
		err := b.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > stableConnection {
			backoff = minBackoff
		}
		if err != nil {
			log.Printf("connection: %v", err)
		}
		b.SaveState()
		log.Printf("reconnecting in %v", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// stableConnection: a connection that stayed up this long resets the
	// backoff, so the next outage is retried quickly again.
	stableConnection = 10 * time.Second
	// dialTimeout bounds TCP connect, TLS handshake and the initial sync, so
	// a server that accepts connections but never answers cannot stall us.
	dialTimeout = 20 * time.Second
	// doTimeout bounds how long we wait for gumble's client lock. gumble can
	// deadlock its reader on malformed server input; if that happens the
	// connection is abandoned and a new one is made.
	doTimeout = 10 * time.Second
)

var errUnresponsive = errors.New("gumble client unresponsive")

// doWithTimeout runs f under the client lock, giving up after timeout. A
// panic in f is logged and reported as failure instead of killing the
// process.
func doWithTimeout(c *gumble.Client, timeout time.Duration, f func()) bool {
	done := make(chan bool, 1)
	go func() {
		ok := false
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in client call: %v\n%s", r, debug.Stack())
			}
			done <- ok
		}()
		c.Do(f)
		ok = true
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func (b *Bot) persistLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.persist:
			func() {
				defer logPanic("save state")
				b.SaveState()
			}()
		}
	}
}

func (b *Bot) runOnce(ctx context.Context) (err error) {
	var client *gumble.Client
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v\n%s", r, debug.Stack())
			if client != nil {
				client.Conn.Close()
			}
			err = fmt.Errorf("recovered from panic: %v", r)
		}
	}()

	tlsConfig, err := b.cfg.tlsConfig()
	if err != nil {
		return err
	}
	disconnected := make(chan *gumble.DisconnectEvent, 1)

	// Events are only accepted while this attempt is current. If gumble's
	// sync timeout fires just as the server finally answers, the abandoned
	// connection may still deliver events; they must not reach the manager.
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	gate := func(f func()) {
		if active.Load() {
			f()
		}
	}

	gcfg := gumble.NewConfig()
	gcfg.Username = b.cfg.Username
	gcfg.Password = b.cfg.Password
	gcfg.Tokens = b.cfg.Tokens
	gcfg.Attach(gumbleutil.Listener{
		Connect: func(e *gumble.ConnectEvent) {
			gate(func() { b.onConnect(e) })
		},
		UserChange: func(e *gumble.UserChangeEvent) {
			gate(func() { b.onUserChange(e) })
		},
		ChannelChange: func(e *gumble.ChannelChangeEvent) {
			gate(func() { b.onChannelChange(e) })
		},
		ServerConfig: func(e *gumble.ServerConfigEvent) {
			gate(func() { b.sender.SetServerConfig(e) })
		},
		PermissionDenied: func(e *gumble.PermissionDeniedEvent) {
			log.Printf("permission denied: %s", describeDenied(e))
		},
		Disconnect: func(e *gumble.DisconnectEvent) {
			select {
			case disconnected <- e:
			default:
			}
		},
	})

	log.Printf("connecting to %s as %q", b.cfg.Address(), b.cfg.Username)
	dialer := &net.Dialer{Timeout: dialTimeout}
	client, err = gumble.DialWithDialer(dialer, b.cfg.Address(), gcfg, tlsConfig)
	if err != nil {
		return err
	}
	b.sender.SetClient(client)
	defer b.sender.SetClient(nil)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			client.Disconnect()
			return nil
		case e := <-disconnected:
			if e.String != "" {
				return fmt.Errorf("disconnected: %s", e.String)
			}
			return errors.New("disconnected")
		case now := <-tick.C:
			if client.State() == gumble.StateDisconnected {
				return errors.New("disconnected")
			}
			if err := b.execute(client, b.mgr.Tick(now)); err != nil {
				client.Conn.Close()
				return err
			}
			// Probe the client lock even when there is nothing to do, so a
			// wedged client is noticed promptly.
			if !doWithTimeout(client, doTimeout, func() {}) {
				client.Conn.Close()
				return errUnresponsive
			}
		}
	}
}

func (b *Bot) onConnect(e *gumble.ConnectEvent) {
	defer logPanic("connect")
	c := e.Client
	if c.Self == nil {
		log.Print("server did not tell us our own session; reconnecting")
		c.Conn.Close()
		return
	}
	log.Printf("connected; bot session %d", c.Self.Session)

	c.Self.SetSelfDeafened(true)
	if b.cfg.Register && !c.Self.IsRegistered() {
		log.Printf("registering bot user %q", c.Self.Name)
		c.Self.Register()
	}
	if b.cfg.HomeChannel != 0 {
		if ch := c.Channels[b.cfg.HomeChannel]; ch != nil && c.Self.Channel != ch {
			c.Self.Move(ch)
		}
	}

	var snap micseq.Snapshot
	for _, ch := range c.Channels {
		snap.Channels = append(snap.Channels, micseq.ChannelInfo{ID: ch.ID, Name: ch.Name})
	}
	for _, u := range c.Users {
		snap.Users = append(snap.Users, b.userInfo(c, u))
	}
	b.execute(c, b.mgr.Sync(snap, time.Now()))
}

func (b *Bot) onUserChange(e *gumble.UserChangeEvent) {
	defer logPanic("user change")
	c := e.Client
	if e.Type.Has(gumble.UserChangeDisconnected) {
		b.execute(c, b.mgr.Handle(micseq.UserGone{Session: e.User.Session}, time.Now()))
		return
	}
	if e.User.Channel == nil {
		return
	}
	actor := micseq.ActorNone
	switch {
	case e.Actor == nil:
	case c.Self != nil && e.Actor.Session == c.Self.Session:
		actor = micseq.ActorSelf
	default:
		actor = micseq.ActorOther
	}
	ev := micseq.UserChanged{User: b.userInfo(c, e.User), Actor: actor}
	b.execute(c, b.mgr.Handle(ev, time.Now()))
}

func (b *Bot) onChannelChange(e *gumble.ChannelChangeEvent) {
	defer logPanic("channel change")
	var ev micseq.Event
	switch {
	case e.Type.Has(gumble.ChannelChangeRemoved):
		ev = micseq.ChannelGone{ID: e.Channel.ID}
	case e.Type.Has(gumble.ChannelChangeCreated) || e.Type.Has(gumble.ChannelChangeName):
		ev = micseq.ChannelChanged{Channel: micseq.ChannelInfo{ID: e.Channel.ID, Name: e.Channel.Name}}
	default:
		return
	}
	b.execute(e.Client, b.mgr.Handle(ev, time.Now()))
}

func (b *Bot) userInfo(c *gumble.Client, u *gumble.User) micseq.UserInfo {
	info := micseq.UserInfo{
		Session:    u.Session,
		UserID:     u.UserID,
		Name:       u.Name,
		Muted:      u.Muted,
		Deafened:   u.Deafened,
		Suppressed: u.Suppressed,
	}
	if u.Channel != nil {
		info.ChannelID = u.Channel.ID
	}
	// SuperUser (user ID 0) cannot be moved or muted by anyone else.
	isSuperUser := u.UserID == 0 && strings.EqualFold(u.Name, "SuperUser")
	info.Ignored = (c.Self != nil && u.Session == c.Self.Session) || isSuperUser || b.ignore[strings.ToLower(u.Name)]
	return info
}

// logPanic keeps a bug in an event handler from killing the process: gumble
// calls listeners on its reader goroutine, where a panic is fatal.
func logPanic(where string) {
	if r := recover(); r != nil {
		log.Printf("panic in %s handler: %v\n%s", where, r, debug.Stack())
	}
}

// execute carries out manager actions. Users and channels are looked up again
// by ID because gumble objects of removed users/channels must not be used.
func (b *Bot) execute(c *gumble.Client, actions []micseq.Action) error {
	var server []micseq.Action
	for _, a := range actions {
		switch a := a.(type) {
		case micseq.SayChannel:
			b.sender.ToChannel(a.ChannelID, a.HTML, a.Coalesce)
		case micseq.SayUser:
			b.sender.ToUser(c, a.Session, a.HTML)
		case micseq.Persist:
			select {
			case b.persist <- struct{}{}:
			default:
			}
		default:
			server = append(server, a)
		}
	}
	if len(server) == 0 {
		return nil
	}
	ok := doWithTimeout(c, doTimeout, func() {
		for _, a := range server {
			switch a := a.(type) {
			case micseq.Unsuppress:
				if u := c.Users[a.Session]; u != nil {
					log.Printf("unsuppress %q", u.Name)
					u.SetSuppressed(false)
				}
			case micseq.Move:
				u, ch := c.Users[a.Session], c.Channels[a.ChannelID]
				if u != nil && ch != nil {
					log.Printf("move %q to %q", u.Name, ch.Name)
					u.Move(ch)
				}
			case micseq.SetMute:
				if u := c.Users[a.Session]; u != nil {
					log.Printf("set mute %q = %v", u.Name, a.Muted)
					u.SetMuted(a.Muted)
				}
			}
		}
	})
	if !ok {
		return errUnresponsive
	}
	return nil
}

func describeDenied(e *gumble.PermissionDeniedEvent) string {
	var parts []string
	switch e.Type {
	case gumble.PermissionDeniedPermission:
		parts = append(parts, fmt.Sprintf("missing permission %d", e.Permission))
	case gumble.PermissionDeniedSuperUser:
		parts = append(parts, "target is SuperUser")
	case gumble.PermissionDeniedChannelFull:
		parts = append(parts, "channel full")
	case gumble.PermissionDeniedTextTooLong:
		parts = append(parts, "text too long")
	case gumble.PermissionDeniedTemporaryChannel:
		parts = append(parts, "temporary channel")
	case gumble.PermissionDeniedMissingCertificate:
		parts = append(parts, "missing certificate")
	default:
		parts = append(parts, fmt.Sprintf("type %d", e.Type))
	}
	if e.Channel != nil {
		parts = append(parts, fmt.Sprintf("channel %q", e.Channel.Name))
	}
	if e.User != nil {
		parts = append(parts, fmt.Sprintf("user %q", e.User.Name))
	}
	if e.String != "" {
		parts = append(parts, e.String)
	}
	return strings.Join(parts, ", ")
}
