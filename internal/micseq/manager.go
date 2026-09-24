package micseq

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// Config tunes the manager. Zero intervals disable the matching announcement.
type Config struct {
	// QueueInterval is how often the full status (speaker, remaining time and
	// queue) is posted.
	QueueInterval time.Duration
	// RemainingInterval is how often the speaker's remaining time is posted.
	RemainingInterval time.Duration
	// WarnBefore is how long before the end of a turn the next user is told
	// to get ready.
	WarnBefore time.Duration
	// ConfirmTimeout is how long to wait for the server to confirm an
	// unsuppress or a move.
	ConfirmTimeout time.Duration
	// ResuppressBackoff is the minimum gap between unsuppress retries for a
	// speaker the server suppressed again.
	ResuppressBackoff time.Duration
	// PersistInterval is how often state is saved while someone is speaking.
	PersistInterval time.Duration
	// RejoinGrace is how long a disconnected queued user keeps their place.
	// Zero removes them immediately.
	RejoinGrace time.Duration
	// SpeakerOfflineWait is how long the queue is held when the speaker goes
	// offline (or is missing after the bot reconnects, e.g. after a server
	// restart). Zero ends the turn immediately.
	SpeakerOfflineWait time.Duration
	// RejoinBonus is extra speaking time given to a speaker who comes back
	// after going offline.
	RejoinBonus time.Duration
	// RejoinWindow is how long after losing the floor for being offline a
	// speaker may come back and be put right after the current speaker, to
	// finish their remaining time.
	RejoinWindow time.Duration
	// Less orders users whose join order was never observed.
	Less func(a, b string) bool
}

// DefaultConfig returns the default timings.
func DefaultConfig() Config {
	return Config{
		QueueInterval:      60 * time.Second,
		RemainingInterval:  60 * time.Second,
		WarnBefore:         30 * time.Second,
		ConfirmTimeout:     5 * time.Second,
		ResuppressBackoff:  3 * time.Second,
		PersistInterval:    5 * time.Second,
		RejoinGrace:        90 * time.Second,
		SpeakerOfflineWait: time.Minute,
		RejoinBonus:        30 * time.Second,
		RejoinWindow:       10 * time.Minute,
		Less:               NameLess(true),
	}
}

// maxTickStep bounds how much speaking time a single tick can consume, so a
// stalled process or a suspended machine does not eat whole turns.
const maxTickStep = 5 * time.Second

type member struct {
	Key  string
	Name string
	// Resume is set for a speaker who lost the floor while offline and came
	// back within RejoinWindow: the time they had left.
	Resume time.Duration
}

// droppedSpeaker remembers a speaker whose turn was given away because they
// stayed offline too long.
type droppedSpeaker struct {
	Name      string
	Remaining time.Duration
	Until     time.Time
}

type userEntry struct {
	UserInfo
	// joinSeq orders observed channel entries; 0 means the entry was not
	// observed (the user was already there when the bot connected).
	joinSeq uint64
}

type pendingMove struct {
	name     string
	from     uint32
	target   uint32
	deadline time.Time
}

type channelState struct {
	id     uint32
	marker Marker

	queue    []member
	exempt   map[string]string    // unsuppressed by an admin; left alone
	finished map[string]string    // turn over, move failed, muted by the bot
	away     map[string]time.Time // disconnected members: keep place until
	dropped  map[string]droppedSpeaker

	speaker        *member
	remaining      time.Duration
	paused         bool
	warned         bool
	confirmed      bool
	turnStart      time.Time
	lastResuppress time.Time

	lastQueueAnnounce  time.Time
	lastRemainAnnounce time.Time

	permWarned      bool
	targetWarnedFor uint32 // target ID already warned about; 0 = none
	restored        bool
}

// Manager is the 麦序 state machine. It never talks to the server itself:
// callers feed it events and execute the returned actions. It is safe for
// concurrent use.
type Manager struct {
	mu  sync.Mutex
	cfg Config

	users         map[uint32]*userEntry
	channels      map[uint32]string
	managed       map[uint32]*channelState
	pending       map[string]*pendingMove
	invalidWarned map[uint32]string

	joinSeq     uint64
	lastTick    time.Time
	lastPersist time.Time
	dirty       bool
	synced      bool

	out []Action
}

// NewManager creates a manager.
func NewManager(cfg Config) *Manager {
	if cfg.Less == nil {
		cfg.Less = NameLess(false)
	}
	return &Manager{
		cfg:           cfg,
		users:         map[uint32]*userEntry{},
		channels:      map[uint32]string{},
		managed:       map[uint32]*channelState{},
		pending:       map[string]*pendingMove{},
		invalidWarned: map[uint32]string{},
	}
}

func (m *Manager) emit(a Action) { m.out = append(m.out, a) }

func (m *Manager) sayChan(id uint32, text string) {
	m.emit(SayChannel{ChannelID: id, HTML: text})
}

func (m *Manager) flush() []Action {
	out := m.out
	m.out = nil
	return out
}

// Sync replaces the manager's view of the server with a fresh snapshot taken
// right after connecting. Channel states survive, so a reconnect or a restart
// with restored state resumes where it left off.
func (m *Manager) Sync(s Snapshot, now time.Time) []Action {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.users = map[uint32]*userEntry{}
	for _, u := range s.Users {
		m.users[u.Session] = &userEntry{UserInfo: u}
	}
	m.channels = map[uint32]string{}
	for _, c := range s.Channels {
		m.channels[c.ID] = c.Name
	}
	m.lastTick = now
	m.synced = true

	for key, p := range m.pending {
		p.deadline = now.Add(m.cfg.ConfirmTimeout)
		u := m.userByKey(key)
		if _, ok := m.managed[p.from]; !ok || u == nil || u.ChannelID != p.from {
			delete(m.pending, key)
		}
	}
	for id, cs := range m.managed {
		if _, ok := m.channels[id]; !ok {
			delete(m.managed, id)
			m.dirty = true
			continue
		}
		if cs.speaker != nil {
			cs.confirmed = false
			cs.turnStart = now
			cs.lastResuppress = time.Time{}
		}
		// After a reconnect (for example a server restart) users come back
		// one by one; keep everyone's place for a while.
		present := m.presentIn(id)
		for _, key := range cs.memberKeys() {
			if _, ok := present[key]; ok {
				delete(cs.away, key)
			} else {
				m.markAway(cs, key, now)
			}
		}
	}
	for _, id := range sortedIDs(m.channels) {
		m.refreshChannel(id, now)
	}
	for _, id := range sortedIDs(m.managed) {
		cs := m.managed[id]
		if cs.speaker != nil && m.speakerAway(cs) {
			m.sayChan(id, msgSpeakerOffline(cs.speaker.Name, m.cfg.SpeakerOfflineWait, cs.remaining))
		}
	}
	return m.flush()
}

// markAway starts the offline wait for a speaker or queued member who is
// not in the channel. It reports whether the member is now being waited for.
func (m *Manager) markAway(cs *channelState, key string, now time.Time) bool {
	wait := m.cfg.RejoinGrace
	if cs.speaker != nil && cs.speaker.Key == key {
		wait = m.cfg.SpeakerOfflineWait
	} else if !cs.inQueue(key) {
		return false
	}
	if wait <= 0 {
		return false
	}
	cs.away[key] = now.Add(wait)
	return true
}

// Handle processes one server event.
func (m *Manager) Handle(ev Event, now time.Time) []Action {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.synced {
		return nil
	}
	switch e := ev.(type) {
	case UserChanged:
		m.userChanged(e, now)
	case UserGone:
		m.userGone(e.Session, now)
	case ChannelChanged:
		m.channels[e.Channel.ID] = e.Channel.Name
		m.refreshChannel(e.Channel.ID, now)
	case ChannelGone:
		delete(m.channels, e.ID)
		delete(m.invalidWarned, e.ID)
		if _, ok := m.managed[e.ID]; ok {
			delete(m.managed, e.ID)
			m.dirty = true
		}
	}
	return m.flush()
}

func (m *Manager) userChanged(e UserChanged, now time.Time) {
	u := e.User
	old := m.users[u.Session]
	entry := &userEntry{UserInfo: u}
	switch {
	case old == nil || old.ChannelID != u.ChannelID:
		m.joinSeq++
		entry.joinSeq = m.joinSeq
	default:
		entry.joinSeq = old.joinSeq
	}
	m.users[u.Session] = entry
	key := u.Key()

	if old != nil && old.Key() != key {
		m.renameKey(old.Key(), key, u.Name)
	}
	if p, ok := m.pending[key]; ok && u.ChannelID != p.from {
		delete(m.pending, key)
	}

	if old != nil && old.ChannelID == u.ChannelID && old.Suppressed && !u.Suppressed && e.Actor == ActorOther {
		if cs := m.managed[u.ChannelID]; cs != nil && cs.removeQueued(key) {
			cs.exempt[key] = u.Name
			m.sayChan(cs.id, msgExempt(u.Name))
			m.dirty = true
		}
	}

	if old != nil && old.ChannelID != u.ChannelID {
		if cs := m.managed[old.ChannelID]; cs != nil {
			m.reconcile(cs, now)
		}
	}
	if cs := m.managed[u.ChannelID]; cs != nil {
		m.reconcile(cs, now)
	}
}

func (m *Manager) userGone(session uint32, now time.Time) {
	old := m.users[session]
	if old == nil {
		return
	}
	delete(m.users, session)
	key := old.Key()
	delete(m.pending, key)
	cs := m.managed[old.ChannelID]
	if cs == nil {
		return
	}
	// A disconnect may be temporary (a network hiccup or a server restart).
	// Queued users keep their place for RejoinGrace; the speaker's turn is
	// held for SpeakerOfflineWait with the clock stopped.
	if m.markAway(cs, key, now) && cs.speaker != nil && cs.speaker.Key == key {
		m.sayChan(cs.id, msgSpeakerOffline(cs.speaker.Name, m.cfg.SpeakerOfflineWait, cs.remaining))
	}
	m.reconcile(cs, now)
}

func (m *Manager) renameKey(oldKey, newKey, name string) {
	for _, cs := range m.managed {
		for i := range cs.queue {
			if cs.queue[i].Key == oldKey {
				cs.queue[i].Key, cs.queue[i].Name = newKey, name
			}
		}
		if cs.speaker != nil && cs.speaker.Key == oldKey {
			cs.speaker = &member{Key: newKey, Name: name}
		}
		for _, set := range []map[string]string{cs.exempt, cs.finished} {
			if _, ok := set[oldKey]; ok {
				delete(set, oldKey)
				set[newKey] = name
			}
		}
		if t, ok := cs.away[oldKey]; ok {
			delete(cs.away, oldKey)
			cs.away[newKey] = t
		}
		if d, ok := cs.dropped[oldKey]; ok {
			delete(cs.dropped, oldKey)
			d.Name = name
			cs.dropped[newKey] = d
		}
	}
	if p, ok := m.pending[oldKey]; ok {
		delete(m.pending, oldKey)
		m.pending[newKey] = p
	}
	m.dirty = true
}

// refreshChannel starts, updates or stops management of a channel according
// to the marker in its name.
func (m *Manager) refreshChannel(id uint32, now time.Time) {
	name := m.channels[id]
	cs := m.managed[id]
	marker, found, err := ParseMarker(name)

	switch {
	case !found:
		delete(m.invalidWarned, id)
		if cs != nil {
			delete(m.managed, id)
			m.sayChan(id, msgStopped())
			m.dirty = true
		}
		return
	case err != nil:
		// Keep running with the previous settings while an admin fixes a typo.
		if m.invalidWarned[id] != name {
			m.invalidWarned[id] = name
			m.sayChan(id, msgInvalidMarker(err))
		}
		if cs != nil {
			m.reconcile(cs, now)
		}
		return
	}
	delete(m.invalidWarned, id)

	if cs == nil {
		cs = newChannelState(id)
		cs.marker = marker
		m.managed[id] = cs
		cs.lastQueueAnnounce = now
		m.sayChan(id, msgTakeover(marker.Duration, m.channelName(marker.TargetID)))
		if open := m.unsuppressedIn(id); len(open) > 0 {
			m.sayChan(id, msgACLOpen(open))
		}
		m.dirty = true
	} else {
		if cs.restored {
			cs.restored = false
			cs.lastQueueAnnounce = now
			m.sayChan(id, msgResumeAfterRestart())
		}
		if cs.marker != marker {
			cs.marker = marker
			m.dirty = true
		}
	}
	m.checkTarget(cs)
	m.reconcile(cs, now)
}

func newChannelState(id uint32) *channelState {
	return &channelState{
		id:       id,
		exempt:   map[string]string{},
		finished: map[string]string{},
		away:     map[string]time.Time{},
		dropped:  map[string]droppedSpeaker{},
	}
}

func (m *Manager) checkTarget(cs *channelState) {
	t := cs.marker.TargetID
	_, exists := m.channels[t]
	if exists && t != cs.id {
		cs.targetWarnedFor = 0
		return
	}
	if cs.targetWarnedFor != t {
		cs.targetWarnedFor = t
		m.sayChan(cs.id, msgBadTarget(t, t == cs.id))
	}
}

func (m *Manager) channelName(id uint32) string {
	if name, ok := m.channels[id]; ok {
		if short := StripMarker(name); short != "" {
			return short
		}
		return name
	}
	return "#" + strconv.FormatUint(uint64(id), 10)
}

func (m *Manager) unsuppressedIn(id uint32) []string {
	var names []string
	for _, u := range m.presentIn(id) {
		if !u.Suppressed {
			names = append(names, u.Name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return m.cfg.Less(names[i], names[j]) })
	return names
}

// presentIn returns the queueable users currently in a channel, by key.
func (m *Manager) presentIn(id uint32) map[string]*userEntry {
	present := map[string]*userEntry{}
	for _, u := range m.users {
		if u.ChannelID == id && !u.Ignored {
			present[u.Key()] = u
		}
	}
	return present
}

func (m *Manager) userByKey(key string) *userEntry {
	for _, u := range m.users {
		if u.Key() == key {
			return u
		}
	}
	return nil
}

// reconcile brings a managed channel's state in line with who is actually
// in it, then makes sure someone is speaking if anyone is waiting.
func (m *Manager) reconcile(cs *channelState, now time.Time) {
	present := m.presentIn(cs.id)

	kept := cs.queue[:0]
	for _, q := range cs.queue {
		if u, ok := present[q.Key]; ok {
			delete(cs.away, q.Key)
			q.Name = u.Name
			kept = append(kept, q)
		} else if cs.isAway(q.Key, now) {
			kept = append(kept, q)
		} else {
			delete(cs.away, q.Key)
			m.dirty = true
		}
	}
	cs.queue = kept

	for key := range cs.exempt {
		if _, ok := present[key]; !ok {
			delete(cs.exempt, key)
			m.dirty = true
		}
	}
	for key := range cs.finished {
		if _, ok := present[key]; !ok {
			delete(cs.finished, key)
			if u := m.userByKey(key); u != nil && u.Muted {
				m.emit(SetMute{Session: u.Session, Muted: false})
			}
			m.dirty = true
		}
	}

	if cs.speaker != nil {
		key := cs.speaker.Key
		_, wasAway := cs.away[key]
		switch _, ok := present[key]; {
		case ok && wasAway:
			delete(cs.away, key)
			cs.confirmed = false
			cs.turnStart = now
			cs.lastResuppress = time.Time{}
			cs.lastRemainAnnounce = now
			cs.remaining += m.cfg.RejoinBonus
			m.sayChan(cs.id, msgRejoined(cs.speaker.Name, m.cfg.RejoinBonus, cs.remaining))
			m.dirty = true
		case ok, cs.isAway(key, now):
		default:
			delete(cs.away, key)
			if wasAway {
				if m.cfg.RejoinWindow > 0 {
					cs.dropped[key] = droppedSpeaker{Name: cs.speaker.Name, Remaining: cs.remaining, Until: now.Add(m.cfg.RejoinWindow)}
				}
				m.sayChan(cs.id, msgSpeakerDropped(cs.speaker.Name, m.cfg.SpeakerOfflineWait, m.cfg.RejoinWindow))
			} else {
				m.sayChan(cs.id, msgLeftEarly(cs.speaker.Name))
			}
			cs.speaker = nil
			m.dirty = true
			m.startNext(cs, now, true)
		}
	}

	for key, d := range cs.dropped {
		if !now.Before(d.Until) {
			delete(cs.dropped, key)
			m.dirty = true
		}
	}

	var missing []*userEntry
	for key, u := range present {
		if cs.isTracked(key) {
			continue
		}
		if p, ok := m.pending[key]; ok && p.from == cs.id {
			continue
		}
		if d, ok := cs.dropped[key]; ok {
			// A speaker who lost the floor while offline is back in time:
			// they go right after the current speaker.
			delete(cs.dropped, key)
			cs.insertResumed(member{Key: key, Name: u.Name, Resume: d.Remaining})
			m.sayChan(cs.id, msgCutIn(u.Name))
			m.dirty = true
			continue
		}
		missing = append(missing, u)
	}
	sort.Slice(missing, func(i, j int) bool {
		a, b := missing[i], missing[j]
		if (a.joinSeq == 0) != (b.joinSeq == 0) {
			return a.joinSeq == 0
		}
		if a.joinSeq == 0 {
			if a.Name != b.Name {
				return m.cfg.Less(a.Name, b.Name)
			}
			return a.Session < b.Session
		}
		return a.joinSeq < b.joinSeq
	})
	for _, u := range missing {
		cs.queue = append(cs.queue, member{Key: u.Key(), Name: u.Name})
		m.dirty = true
	}

	if cs.speaker == nil {
		m.startNext(cs, now, false)
		return
	}

	sp := present[cs.speaker.Key]
	if sp == nil {
		return // disconnected, waiting for them to come back
	}
	cs.speaker.Name = sp.Name
	if sp.Suppressed {
		cs.confirmed = false
		if cs.lastResuppress.IsZero() || now.Sub(cs.lastResuppress) >= m.cfg.ResuppressBackoff {
			cs.lastResuppress = now
			m.emit(Unsuppress{Session: sp.Session})
		}
	} else {
		cs.confirmed = true
	}
	if paused := sp.Muted || sp.Deafened; paused != cs.paused {
		cs.paused = paused
		if paused {
			m.sayChan(cs.id, msgPaused(sp.Name, cs.remaining))
		} else {
			m.sayChan(cs.id, msgResumed(sp.Name, cs.remaining))
		}
		m.dirty = true
	}
}

func (cs *channelState) isTracked(key string) bool {
	if cs.speaker != nil && cs.speaker.Key == key {
		return true
	}
	if _, ok := cs.exempt[key]; ok {
		return true
	}
	if _, ok := cs.finished[key]; ok {
		return true
	}
	for _, q := range cs.queue {
		if q.Key == key {
			return true
		}
	}
	return false
}

func (cs *channelState) inQueue(key string) bool {
	for _, q := range cs.queue {
		if q.Key == key {
			return true
		}
	}
	return false
}

// memberKeys returns the speaker's and all queued users' keys.
func (cs *channelState) memberKeys() []string {
	var keys []string
	if cs.speaker != nil {
		keys = append(keys, cs.speaker.Key)
	}
	for _, q := range cs.queue {
		keys = append(keys, q.Key)
	}
	return keys
}

func (cs *channelState) isAway(key string, now time.Time) bool {
	until, ok := cs.away[key]
	return ok && now.Before(until)
}

func (cs *channelState) hasExpiredAway(now time.Time) bool {
	for _, until := range cs.away {
		if !now.Before(until) {
			return true
		}
	}
	return false
}

// speakerAway reports whether the speaker is disconnected and being waited for.
func (m *Manager) speakerAway(cs *channelState) bool {
	return cs.speaker != nil && m.presentIn(cs.id)[cs.speaker.Key] == nil
}

// nextPresent returns the first queued user who is actually in the channel.
func (m *Manager) nextPresent(cs *channelState) *member {
	present := m.presentIn(cs.id)
	for i := range cs.queue {
		if present[cs.queue[i].Key] != nil {
			return &cs.queue[i]
		}
	}
	return nil
}

// insertResumed puts a returning speaker at the front of the queue, behind
// any other returning speakers who came back earlier.
func (cs *channelState) insertResumed(mb member) {
	i := 0
	for i < len(cs.queue) && cs.queue[i].Resume > 0 {
		i++
	}
	cs.queue = append(cs.queue, member{})
	copy(cs.queue[i+1:], cs.queue[i:])
	cs.queue[i] = mb
}

func (cs *channelState) removeQueued(key string) bool {
	for i, q := range cs.queue {
		if q.Key == key {
			cs.queue = append(cs.queue[:i], cs.queue[i+1:]...)
			return true
		}
	}
	return false
}

// startNext gives the floor to the first queued user who is present;
// disconnected users keep their place until they return or time out.
// announceEmpty controls whether an empty queue is reported (only right
// after a turn ends).
func (m *Manager) startNext(cs *channelState, now time.Time, announceEmpty bool) {
	head := m.nextPresent(cs)
	if head == nil {
		if announceEmpty && len(cs.queue) == 0 {
			m.sayChan(cs.id, msgQueueEmpty())
		}
		return
	}
	u := m.presentIn(cs.id)[head.Key]
	resume := head.Resume
	cs.removeQueued(u.Key())

	cs.speaker = &member{Key: u.Key(), Name: u.Name}
	cs.remaining = cs.marker.Duration
	if resume > 0 {
		cs.remaining = resume + m.cfg.RejoinBonus
	}
	cs.warned = false
	cs.turnStart = now
	cs.lastRemainAnnounce = now
	cs.confirmed = !u.Suppressed
	cs.paused = u.Muted || u.Deafened
	cs.lastResuppress = time.Time{}
	if u.Suppressed {
		cs.lastResuppress = now
		m.emit(Unsuppress{Session: u.Session})
	}
	m.sayChan(cs.id, msgTurnStart(u.Name, cs.remaining, resume > 0, m.nextPresent(cs)))
	if cs.paused {
		m.sayChan(cs.id, msgPaused(u.Name, cs.remaining))
	}
	m.dirty = true
}

// endTurn moves the current speaker out once their time is up.
func (m *Manager) endTurn(cs *channelState, now time.Time) {
	sp := *cs.speaker
	cs.speaker = nil
	m.dirty = true
	u := m.presentIn(cs.id)[sp.Key]
	target := cs.marker.TargetID
	_, targetExists := m.channels[target]
	switch {
	case u == nil:
	case targetExists && target != cs.id:
		m.emit(Move{Session: u.Session, ChannelID: target})
		m.pending[sp.Key] = &pendingMove{
			name:     u.Name,
			from:     cs.id,
			target:   target,
			deadline: now.Add(m.cfg.ConfirmTimeout),
		}
		m.sayChan(cs.id, msgTimeUp(u.Name, m.channelName(target)))
	default:
		m.muteFinished(cs, sp.Key, u)
	}
	m.startNext(cs, now, true)
}

func (m *Manager) muteFinished(cs *channelState, key string, u *userEntry) {
	cs.finished[key] = u.Name
	m.emit(SetMute{Session: u.Session, Muted: true})
	m.sayChan(cs.id, msgMoveFailed(u.Name, m.channelName(cs.marker.TargetID)))
	m.dirty = true
}

// Tick advances timers; call it about once a second.
func (m *Manager) Tick(now time.Time) []Action {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.synced {
		return nil
	}
	elapsed := now.Sub(m.lastTick)
	if elapsed < 0 {
		elapsed = 0
	} else if elapsed > maxTickStep {
		elapsed = maxTickStep
	}
	m.lastTick = now

	speaking := false
	for _, id := range sortedIDs(m.managed) {
		cs := m.managed[id]
		before := cs.speaker
		if cs.hasExpiredAway(now) {
			m.reconcile(cs, now)
		}
		// A turn that started during this tick is not charged for it.
		if cs.speaker != nil && cs.speaker == before {
			m.tickSpeaker(cs, now, elapsed)
		}
		if cs.speaker != nil {
			speaking = true
		}
		m.tickAnnounce(cs, now)
	}

	for key, p := range m.pending {
		if now.Before(p.deadline) {
			continue
		}
		delete(m.pending, key)
		cs := m.managed[p.from]
		u := m.userByKey(key)
		if cs == nil || u == nil || u.ChannelID != p.from {
			continue
		}
		m.muteFinished(cs, key, u)
	}

	if m.dirty || (speaking && now.Sub(m.lastPersist) >= m.cfg.PersistInterval) {
		m.dirty = false
		m.lastPersist = now
		m.emit(Persist{})
	}
	return m.flush()
}

func (m *Manager) tickSpeaker(cs *channelState, now time.Time, elapsed time.Duration) {
	if m.speakerAway(cs) {
		return // the clock stops until they reconnect or the grace period ends
	}
	if !cs.paused {
		cs.remaining -= elapsed
	}
	if !cs.confirmed && !cs.permWarned && now.Sub(cs.turnStart) >= m.cfg.ConfirmTimeout {
		cs.permWarned = true
		m.sayChan(cs.id, msgNoMuteDeafen(cs.speaker.Name))
	}
	if cs.remaining <= 0 {
		m.endTurn(cs, now)
		return
	}
	if next := m.nextPresent(cs); next != nil && !cs.warned && m.cfg.WarnBefore > 0 && cs.remaining <= m.cfg.WarnBefore {
		cs.warned = true
		cs.lastRemainAnnounce = now
		m.sayChan(cs.id, msgWarnNext(cs.speaker.Name, next.Name, cs.remaining))
		if u := m.presentIn(cs.id)[next.Key]; u != nil {
			m.emit(SayUser{Session: u.Session, HTML: msgWarnNextPrivate(m.channelName(cs.id), cs.remaining)})
		}
		m.dirty = true
	}
}

func (m *Manager) tickAnnounce(cs *channelState, now time.Time) {
	if cs.speaker == nil && len(cs.queue) == 0 {
		return
	}
	coalesce := "status:" + strconv.FormatUint(uint64(cs.id), 10)
	away := m.speakerAway(cs)
	if m.cfg.QueueInterval > 0 && now.Sub(cs.lastQueueAnnounce) >= m.cfg.QueueInterval {
		cs.lastQueueAnnounce = now
		cs.lastRemainAnnounce = now
		note := ""
		switch {
		case away:
			note = "等待重新连接"
		case cs.paused:
			note = "已暂停"
		}
		present := m.presentIn(cs.id)
		lines := make([]statusLine, len(cs.queue))
		for i, q := range cs.queue {
			lines[i] = statusLine{Name: q.Name, Offline: present[q.Key] == nil}
		}
		m.emit(SayChannel{
			ChannelID: cs.id,
			HTML:      msgStatus(cs.speaker, cs.remaining, note, lines),
			Coalesce:  coalesce,
		})
		return
	}
	if cs.speaker != nil && !cs.paused && !away && m.cfg.RemainingInterval > 0 && now.Sub(cs.lastRemainAnnounce) >= m.cfg.RemainingInterval {
		cs.lastRemainAnnounce = now
		m.emit(SayChannel{
			ChannelID: cs.id,
			HTML:      msgRemaining(cs.speaker.Name, cs.remaining),
			Coalesce:  coalesce,
		})
	}
}

func sortedIDs[V any](m map[uint32]V) []uint32 {
	ids := make([]uint32, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
