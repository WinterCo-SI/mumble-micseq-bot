package micseq

import (
	"strings"
	"testing"
	"time"
)

const (
	chRoot    = 0
	chManaged = 10
	chTarget  = 20
	chOther   = 30
)

const managedName = "会议室 [麦序/发言时间: 60s/下麦转20]"

type harness struct {
	t     *testing.T
	m     *Manager
	now   time.Time
	chans map[uint32]string
	users map[uint32]UserInfo
	// autoEcho feeds the bot's own actions back as server echoes.
	autoEcho bool
	// noMoves makes the server ignore Move actions (e.g. missing permission).
	noMoves bool
	log     []Action
}

func newHarness(t *testing.T) *harness {
	cfg := DefaultConfig()
	return &harness{
		t:        t,
		m:        NewManager(cfg),
		now:      time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
		chans:    map[uint32]string{chRoot: "Root", chManaged: managedName, chTarget: "休息区", chOther: "大厅"},
		users:    map[uint32]UserInfo{},
		autoEcho: true,
	}
}

func (h *harness) addInitial(session uint32, name string, channel uint32) {
	h.users[session] = UserInfo{Session: session, Name: name, ChannelID: channel, Suppressed: channel == chManaged}
}

func (h *harness) snapshot() Snapshot {
	var s Snapshot
	for _, id := range sortedIDs(h.chans) {
		s.Channels = append(s.Channels, ChannelInfo{ID: id, Name: h.chans[id]})
	}
	for _, id := range sortedIDs(h.users) {
		s.Users = append(s.Users, h.users[id])
	}
	return s
}

func (h *harness) sync() []Action {
	return h.run(h.m.Sync(h.snapshot(), h.now))
}

func (h *harness) run(actions []Action) []Action {
	h.log = append(h.log, actions...)
	all := append([]Action(nil), actions...)
	if !h.autoEcho {
		return all
	}
	for _, a := range actions {
		switch a := a.(type) {
		case Unsuppress:
			u := h.users[a.Session]
			u.Suppressed = false
			all = append(all, h.change(u, ActorSelf)...)
		case Move:
			if h.noMoves {
				continue
			}
			u := h.users[a.Session]
			u.ChannelID = a.ChannelID
			u.Suppressed = a.ChannelID == chManaged
			all = append(all, h.change(u, ActorSelf)...)
		case SetMute:
			u := h.users[a.Session]
			u.Muted = a.Muted
			all = append(all, h.change(u, ActorSelf)...)
		}
	}
	return all
}

func (h *harness) change(u UserInfo, actor Actor) []Action {
	h.users[u.Session] = u
	return h.run(h.m.Handle(UserChanged{User: u, Actor: actor}, h.now))
}

// join simulates a user entering a channel; the server suppresses them in
// the managed channel.
func (h *harness) join(session uint32, name string, channel uint32) []Action {
	u, ok := h.users[session]
	if !ok {
		u = UserInfo{Session: session, Name: name}
	}
	u.ChannelID = channel
	u.Suppressed = channel == chManaged
	return h.change(u, ActorNone)
}

func (h *harness) leave(session uint32) []Action {
	delete(h.users, session)
	return h.run(h.m.Handle(UserGone{Session: session}, h.now))
}

func (h *harness) rename(id uint32, name string) []Action {
	h.chans[id] = name
	return h.run(h.m.Handle(ChannelChanged{Channel: ChannelInfo{ID: id, Name: name}}, h.now))
}

func (h *harness) advance(d time.Duration) []Action {
	var all []Action
	for step := time.Duration(0); step < d; step += time.Second {
		h.now = h.now.Add(time.Second)
		all = append(all, h.run(h.m.Tick(h.now))...)
	}
	return all
}

func (h *harness) speaker() string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	cs := h.m.managed[chManaged]
	if cs == nil || cs.speaker == nil {
		return ""
	}
	return cs.speaker.Name
}

func (h *harness) queue() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	var names []string
	if cs := h.m.managed[chManaged]; cs != nil {
		for _, q := range cs.queue {
			names = append(names, q.Name)
		}
	}
	return names
}

func (h *harness) remaining() time.Duration {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.managed[chManaged].remaining
}

func (h *harness) wantSpeaker(name string) {
	h.t.Helper()
	if got := h.speaker(); got != name {
		h.t.Fatalf("speaker = %q, want %q", got, name)
	}
}

func (h *harness) wantQueue(names ...string) {
	h.t.Helper()
	got := h.queue()
	if strings.Join(got, ",") != strings.Join(names, ",") {
		h.t.Fatalf("queue = %v, want %v", got, names)
	}
}

func countType[T Action](actions []Action) []T {
	var out []T
	for _, a := range actions {
		if v, ok := a.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// channelTexts returns the channel broadcasts; privateTexts the private
// messages to one session.
func channelTexts(actions []Action) []string {
	var out []string
	for _, a := range countType[SayChannel](actions) {
		out = append(out, a.HTML)
	}
	return out
}

func privateTexts(actions []Action, session uint32) []string {
	var out []string
	for _, a := range countType[SayUser](actions) {
		if a.Session == session {
			out = append(out, a.HTML)
		}
	}
	return out
}

func containsAny(texts []string, substr string) bool {
	for _, t := range texts {
		if strings.Contains(t, substr) {
			return true
		}
	}
	return false
}

func hasText(actions []Action, substr string) bool {
	for _, a := range actions {
		switch a := a.(type) {
		case SayChannel:
			if strings.Contains(a.HTML, substr) {
				return true
			}
		case SayUser:
			if strings.Contains(a.HTML, substr) {
				return true
			}
		}
	}
	return false
}

func TestTakeoverSortsUnseenUsersByPinyin(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "张三", chManaged)
	h.addInitial(2, "王五", chManaged)
	h.addInitial(3, "李四", chManaged)
	acts := h.sync()

	h.wantSpeaker("李四")
	h.wantQueue("王五", "张三")
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 3 {
		t.Fatalf("unsuppress actions = %v", u)
	}
	if !hasText(acts, "本频道已启用麦序模式") || !hasText(acts, "轮到 <b>李四</b> 发言") {
		t.Fatalf("missing takeover/turn messages: %v", acts)
	}
	if hasText(acts, "当前没有被禁言") {
		t.Fatal("unexpected ACL warning when everyone is suppressed")
	}
}

func TestJoinOrderIsKept(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Zed", chManaged)
	h.sync()
	h.join(2, "Bob", chManaged)
	h.join(3, "Alice", chManaged)
	h.wantSpeaker("Zed")
	h.wantQueue("Bob", "Alice")
}

func TestLateTakeoverUsesObservedJoinOrder(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室"
	h.addInitial(1, "Zed", chManaged)
	h.sync()
	h.join(2, "Carol", chManaged)
	h.join(3, "Alice", chManaged)
	acts := h.rename(chManaged, managedName)

	h.wantSpeaker("Zed")
	h.wantQueue("Carol", "Alice")
	if !hasText(acts, "本频道已启用麦序模式") {
		t.Fatal("missing takeover message")
	}
}

func TestACLWarningAtTakeover(t *testing.T) {
	h := newHarness(t)
	h.users[1] = UserInfo{Session: 1, Name: "Host", ChannelID: chManaged}
	h.addInitial(2, "Guest", chManaged)
	acts := h.sync()
	if !hasText(acts, "<b>Host</b> 当前没有被禁言") {
		t.Fatalf("missing ACL warning: %v", acts)
	}
	// Hosts still queue like everyone else.
	h.wantSpeaker("Guest")
	h.wantQueue("Host")
}

func TestExcludesIgnoredUsers(t *testing.T) {
	h := newHarness(t)
	h.users[1] = UserInfo{Session: 1, Name: "MicBot", ChannelID: chManaged, Ignored: true}
	h.addInitial(2, "Alice", chManaged)
	h.sync()
	h.wantSpeaker("Alice")
	h.wantQueue()
}

func TestTurnTimesOutAndMovesToTarget(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()

	acts := h.advance(59 * time.Second)
	if len(countType[Move](acts)) != 0 {
		t.Fatal("moved too early")
	}
	acts = h.advance(time.Second)
	moves := countType[Move](acts)
	if len(moves) != 1 || moves[0] != (Move{Session: 1, ChannelID: chTarget}) {
		t.Fatalf("moves = %v", moves)
	}
	if pm := privateTexts(acts, 1); !containsAny(pm, "你的发言时间已到，已转至「休息区」") ||
		!containsAny(pm, "Your time is up; you have been moved to “休息区”") {
		t.Fatalf("missing private time-up message: %v", pm)
	}
	if containsAny(channelTexts(acts), "已转至") {
		t.Fatal("time-up message broadcast to the channel")
	}
	if pm := privateTexts(acts, 2); !containsAny(pm, "轮到你发言了") {
		t.Fatalf("missing private turn-start message: %v", pm)
	}
	h.wantSpeaker("Bob")
	h.wantQueue()
	if h.users[1].ChannelID != chTarget {
		t.Fatal("Alice was not moved")
	}

	// Confirmed move: no mute fallback later.
	acts = h.advance(10 * time.Second)
	if len(countType[SetMute](acts)) != 0 {
		t.Fatalf("unexpected mute: %v", acts)
	}
}

func TestMoveFailureFallsBackToMute(t *testing.T) {
	h := newHarness(t)
	h.noMoves = true
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()

	h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	acts := h.advance(5 * time.Second)
	mutes := countType[SetMute](acts)
	if len(mutes) != 1 || mutes[0] != (SetMute{Session: 1, Muted: true}) {
		t.Fatalf("mutes = %v", mutes)
	}
	if !hasText(acts, "已改为禁言") {
		t.Fatal("missing move-failed message")
	}
	// The bot's own mute of a finished user is not an admin pause, and the
	// finished user is not queued again.
	if hasText(acts, "计时暂停") {
		t.Fatal("bot mute treated as pause")
	}
	h.wantQueue()

	// Leaving the channel lifts the bot's mute.
	acts = h.join(1, "Alice", chOther)
	if m := countType[SetMute](acts); len(m) != 1 || m[0] != (SetMute{Session: 1, Muted: false}) {
		t.Fatalf("expected unmute on leave, got %v", acts)
	}
	// Coming back queues them again.
	h.join(1, "Alice", chManaged)
	h.wantQueue("Alice")
}

func TestMissingTargetMutesImmediately(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 60s/下麦转99]"
	h.addInitial(1, "Alice", chManaged)
	acts := h.sync()
	if !hasText(acts, "下麦转频道 99 不存在") {
		t.Fatalf("missing bad-target warning: %v", acts)
	}
	acts = h.advance(60 * time.Second)
	if len(countType[Move](acts)) != 0 || len(countType[SetMute](acts)) != 1 {
		t.Fatalf("want immediate mute fallback, got %v", acts)
	}
}

func TestSpeakerLeavingEndsTurnEarly(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.advance(10 * time.Second)

	acts := h.join(1, "Alice", chOther)
	if !hasText(acts, "<b>Alice</b> 已离开频道") {
		t.Fatal("missing left-early message")
	}
	h.wantSpeaker("Bob")
	if h.remaining() != 60*time.Second {
		t.Fatalf("remaining = %v", h.remaining())
	}

	acts = h.leave(2)
	h.wantSpeaker("Bob") // the queue waits for an offline speaker
	if !hasText(acts, "<b>Bob</b> 已离线，麦序暂停，等待其重新连接（最多 1分钟") {
		t.Fatalf("missing offline message: %v", acts)
	}
	acts = h.advance(59 * time.Second)
	h.wantSpeaker("Bob")
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatal("floor given away during the offline wait")
	}
	acts = h.advance(time.Second)
	h.wantSpeaker("Carol")
	if !hasText(acts, "<b>Bob</b> 离线超过 1分钟，轮到下一位") {
		t.Fatalf("missing disconnect message: %v", acts)
	}
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 3 {
		t.Fatalf("unsuppress = %v", u)
	}

	acts = h.join(3, "Carol", chOther)
	h.wantSpeaker("")
	if !hasText(acts, "当前无人排队") {
		t.Fatal("missing empty-queue message")
	}

	// Next person to enter gets the floor right away.
	h.join(4, "Dave", chManaged)
	h.wantSpeaker("Dave")
}

func TestQueuedUserMovingOutIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.join(2, "Bob", chOther)
	h.wantQueue("Carol")
}

func TestDisconnectedQueuedUserKeepsPlace(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()

	h.leave(2)
	h.wantQueue("Bob", "Carol")
	acts := h.advance(60 * time.Second)
	// Bob is skipped while offline; he keeps the head of the queue.
	h.wantSpeaker("Carol")
	h.wantQueue("Bob")
	for _, pm := range countType[SayUser](acts) {
		if strings.Contains(pm.HTML, "下一位发言者") && pm.Session != 3 {
			t.Fatalf("next-speaker reminder to session %d, want only Carol", pm.Session)
		}
	}

	// He reconnects (new session) within the grace period and is next.
	h.join(4, "Dave", chManaged)
	h.join(12, "Bob", chManaged)
	h.wantQueue("Bob", "Dave")

	acts = h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 12 {
		t.Fatalf("unsuppress = %v", u)
	}
}

func TestDisconnectedQueuedUserExpires(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 10分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.leave(2)
	h.advance(89 * time.Second)
	h.wantQueue("Bob", "Carol")
	h.advance(time.Second)
	h.wantQueue("Carol")
	// Coming back later means queueing again at the end.
	h.join(12, "Bob", chManaged)
	h.wantQueue("Carol", "Bob")
}

func TestStatusShowsOfflineUsers(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 10分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.leave(2)
	acts := h.advance(60 * time.Second)
	if !hasText(acts, `<td>Bob</td><td><font color="#d64545">断线，等待重连</font>`) {
		t.Fatalf("status = %s", joinText(acts))
	}
}

func TestRejoinGraceDisabled(t *testing.T) {
	h := newHarness(t)
	h.m.cfg.RejoinGrace = 0
	h.m.cfg.SpeakerOfflineWait = 0
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.leave(2)
	h.wantQueue("Carol")
	h.leave(1)
	h.wantSpeaker("Carol")
}

// restartServer simulates a server restart: the server announces every user
// leaving, the bot's connection drops, and after downtime the bot reconnects
// to an empty server that users rejoin one by one with new sessions.
func (h *harness) restartServer(downtime time.Duration) []Action {
	var acts []Action
	for _, id := range sortedIDs(h.users) {
		acts = append(acts, h.leave(id)...)
	}
	h.now = h.now.Add(downtime)
	return append(acts, h.sync()...)
}

func TestServerRestartKeepsSpeakerAndQueue(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.advance(10 * time.Second)

	acts := h.restartServer(20 * time.Second)
	if hasText(acts, "离线超过") || hasText(acts, "轮到") {
		t.Fatalf("restart ended the turn: %s", joinText(acts))
	}
	if !hasText(acts, "<b>Alice</b> 已离线，麦序暂停，等待其重新连接（最多 1分钟，剩余发言时间 50秒）") {
		t.Fatalf("missing waiting message: %s", joinText(acts))
	}
	h.wantSpeaker("Alice")
	h.wantQueue("Bob", "Carol")

	// Time does not run while the speaker is gone.
	h.advance(15 * time.Second)
	if h.remaining() != 50*time.Second {
		t.Fatalf("remaining = %v", h.remaining())
	}

	// Users come back in a different order.
	acts = h.join(13, "Carol", chManaged)
	acts = append(acts, h.join(12, "Bob", chManaged)...)
	h.wantSpeaker("Alice")
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatalf("gave the floor away while waiting for the speaker: %v", acts)
	}
	acts = h.join(11, "Alice", chRoot) // lands in the default channel first
	acts = append(acts, h.join(11, "Alice", chManaged)...)
	h.wantSpeaker("Alice")
	h.wantQueue("Bob", "Carol")
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 11 {
		t.Fatalf("unsuppress = %v", u)
	}
	if !hasText(acts, "<b>Alice</b> 已重新连接，计时继续，补偿 30秒（剩余 1分20秒）") {
		t.Fatalf("missing rejoin message: %s", joinText(acts))
	}

	acts = h.advance(79 * time.Second)
	if len(countType[Move](acts)) != 0 {
		t.Fatal("rejoin bonus not applied")
	}
	acts = h.advance(time.Second)
	if m := countType[Move](acts); len(m) != 1 || m[0].Session != 11 {
		t.Fatalf("moves = %v", m)
	}
	h.wantSpeaker("Bob")
}

func TestServerRestartSpeakerNeverReturns(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.restartServer(5 * time.Second)
	h.join(12, "Bob", chManaged)

	acts := h.advance(59 * time.Second)
	h.wantSpeaker("Alice")
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatal("floor given away early")
	}
	acts = h.advance(time.Second)
	h.wantSpeaker("Bob")
	if !hasText(acts, "<b>Alice</b> 离线超过 1分钟，轮到下一位") {
		t.Fatalf("missing message: %s", joinText(acts))
	}
}

func TestServerRestartDropsStalePendingMove(t *testing.T) {
	h := newHarness(t)
	h.noMoves = true
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.advance(60 * time.Second) // move sent, not yet confirmed
	h.restartServer(10 * time.Second)
	h.join(11, "Alice", chManaged)
	acts := h.advance(10 * time.Second)
	if len(countType[SetMute](acts)) != 0 {
		t.Fatalf("muted a user after the restart: %v", acts)
	}
}

func TestAdminMutePausesTimer(t *testing.T) {
	for _, deaf := range []bool{false, true} {
		h := newHarness(t)
		h.addInitial(1, "Alice", chManaged)
		h.addInitial(2, "Bob", chManaged)
		h.sync()
		h.advance(20 * time.Second)

		u := h.users[1]
		u.Muted, u.Deafened = true, deaf
		acts := h.change(u, ActorOther)
		if !hasText(acts, "计时暂停（剩余 40秒）") {
			t.Fatalf("deaf=%v: missing pause message: %v", deaf, acts)
		}
		acts = h.advance(5 * time.Minute)
		if len(countType[Move](acts)) != 0 {
			t.Fatalf("deaf=%v: moved while paused", deaf)
		}
		if hasText(acts, "剩余发言时间") {
			t.Fatalf("deaf=%v: remaining-time messages while paused", deaf)
		}
		if h.remaining() != 40*time.Second {
			t.Fatalf("deaf=%v: remaining = %v", deaf, h.remaining())
		}

		u.Muted, u.Deafened = false, false
		acts = h.change(u, ActorOther)
		if !hasText(acts, "计时继续（剩余 40秒）") {
			t.Fatalf("deaf=%v: missing resume message", deaf)
		}
		acts = h.advance(40 * time.Second)
		if len(countType[Move](acts)) != 1 {
			t.Fatalf("deaf=%v: expected move after resume", deaf)
		}
	}
}

func TestSelfMuteDoesNotPause(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	// SelfMuted is not part of UserInfo; a change event with no server mute
	// must not pause.
	h.change(h.users[1], ActorSelf)
	h.advance(10 * time.Second)
	if h.remaining() != 50*time.Second {
		t.Fatalf("remaining = %v", h.remaining())
	}
}

func TestResuppressedSpeakerIsUnsuppressedAgainWithBackoff(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	h.autoEcho = false
	h.advance(5 * time.Second)

	u := h.users[1]
	u.Suppressed = true
	acts := h.change(u, ActorNone)
	if len(countType[Unsuppress](acts)) != 1 {
		t.Fatalf("want unsuppress retry, got %v", acts)
	}
	// Another suppress broadcast within the backoff window: no retry.
	acts = h.change(u, ActorNone)
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatalf("retry inside backoff: %v", acts)
	}
	h.advance(3 * time.Second)
	acts = h.change(u, ActorNone)
	if len(countType[Unsuppress](acts)) != 1 {
		t.Fatalf("want retry after backoff, got %v", acts)
	}
}

func TestUnconfirmedUnsuppressWarnsOnce(t *testing.T) {
	h := newHarness(t)
	h.autoEcho = false
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	acts := h.advance(10 * time.Second)
	if n := strings.Count(joinText(acts), "无法解除"); n != 1 {
		t.Fatalf("MuteDeafen warnings = %d", n)
	}
}

func joinText(actions []Action) string {
	var sb strings.Builder
	for _, a := range actions {
		switch a := a.(type) {
		case SayChannel:
			sb.WriteString(a.HTML + "\n")
		case SayUser:
			sb.WriteString(a.HTML + "\n")
		}
	}
	return sb.String()
}

func TestAdminUnsuppressExemptsQueuedUser(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()

	// A server-side recalculation (no actor) does not count.
	u := h.users[3]
	u.Suppressed = false
	h.change(u, ActorNone)
	h.wantQueue("Bob", "Carol")
	u.Suppressed = true
	h.change(u, ActorNone)

	// An admin unsuppressing Bob exempts him.
	bob := h.users[2]
	bob.Suppressed = false
	acts := h.change(bob, ActorOther)
	if !hasText(acts, "<b>Bob</b> 已由管理员直接授予发言权限") {
		t.Fatalf("missing exempt message: %v", acts)
	}
	h.wantQueue("Carol")

	// Re-suppression later (e.g. an ACL edit) is ignored.
	bob.Suppressed = true
	acts = h.change(bob, ActorNone)
	h.wantQueue("Carol")
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatal("bot touched an exempt user")
	}

	// Bob is never given a turn, moved or announced.
	acts = h.advance(2 * time.Minute)
	for _, mv := range countType[Move](acts) {
		if mv.Session == 2 {
			t.Fatal("exempt user moved")
		}
	}
	if strings.Contains(joinText(acts), "Bob") {
		t.Fatalf("exempt user announced: %s", joinText(acts))
	}

	// Both other turns are over by now. Leaving and coming back makes Bob
	// queue normally again, so he gets the free floor.
	h.wantSpeaker("")
	h.join(2, "Bob", chOther)
	h.join(2, "Bob", chManaged)
	h.wantSpeaker("Bob")
}

func TestNextSpeakerRemindersAreTiered(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 10分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()

	// Checkpoints: 10m (Bob becomes next with 9m59s left), 5m, 2m, 1m, 30s, 10s.
	var got []string
	for elapsed := time.Second; elapsed < 10*time.Minute; elapsed += time.Second {
		acts := h.advance(time.Second)
		for _, pm := range privateTexts(acts, 2) {
			if !strings.Contains(pm, "你是「会议室」的下一位发言者") || !strings.Contains(pm, "You are next in “会议室”") {
				t.Fatalf("unexpected private message: %s", pm)
			}
			got = append(got, FormatDuration(10*time.Minute-elapsed))
		}
		if containsAny(channelTexts(acts), "请做好准备") {
			t.Fatal("next-speaker reminder broadcast to the channel")
		}
	}
	want := "9分59秒,5分钟,2分钟,1分钟,30秒,10秒"
	if strings.Join(got, ",") != want {
		t.Fatalf("next-speaker reminders at %v, want %s", got, want)
	}
}

func TestNextSpeakerWarnedWhenJoiningLate(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	h.advance(40 * time.Second)
	h.join(2, "Bob", chManaged)
	acts := h.advance(time.Second)
	if pm := privateTexts(acts, 2); len(pm) != 1 || !strings.Contains(pm[0], `大约 <font color="#d64545"><b>19秒</b></font> 后轮到你`) {
		t.Fatalf("late next-speaker reminder = %v", pm)
	}
	// The next checkpoint (10s) still follows.
	acts = h.advance(9 * time.Second)
	if pm := privateTexts(acts, 2); len(pm) != 1 || !strings.Contains(pm[0], "<b>10秒</b>") {
		t.Fatalf("10s next-speaker reminder = %v", pm)
	}
}

func TestSpeakerRemindersAreTieredAndPrivate(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 10分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.sync()

	var got []string
	for elapsed := time.Second; elapsed < 10*time.Minute; elapsed += time.Second {
		acts := h.advance(time.Second)
		for _, pm := range privateTexts(acts, 1) {
			if !strings.Contains(pm, "你的发言时间还剩") || !strings.Contains(pm, " left.") {
				t.Fatalf("unexpected private message: %s", pm)
			}
			got = append(got, FormatDuration(10*time.Minute-elapsed))
		}
		if containsAny(channelTexts(acts), "还剩") {
			t.Fatal("remaining time broadcast to the channel")
		}
	}
	// No reminder for the full 10 minutes: the turn-start message says so.
	want := "5分钟,3分钟,2分钟,1分钟,30秒,10秒"
	if strings.Join(got, ",") != want {
		t.Fatalf("speaker reminders at %v, want %s", got, want)
	}
}

func TestSpeakerRemindersRestartAfterRejoinBonus(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	h.advance(35 * time.Second) // 30s reminder sent, 25s left
	h.leave(1)
	h.join(11, "Alice", chManaged) // 25s + 30s bonus = 55s
	acts := h.advance(25 * time.Second)
	if pm := privateTexts(acts, 11); len(pm) != 1 || !strings.Contains(pm[0], "<b>30秒</b>") {
		t.Fatalf("reminders after the bonus = %v", pm)
	}
}

func TestRemindersDisabled(t *testing.T) {
	h := newHarness(t)
	h.m.cfg.SpeakerReminders = nil
	h.m.cfg.NextReminders = nil
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	acts := h.advance(59 * time.Second)
	if pm := countType[SayUser](acts); len(pm) != 0 {
		t.Fatalf("reminders sent while disabled: %v", pm)
	}
}

func TestPeriodicStatus(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 10分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()

	acts := h.advance(59 * time.Second)
	if says := countType[SayChannel](acts); len(says) != 0 {
		t.Fatalf("channel messages before the status interval: %v", says)
	}
	acts = h.advance(time.Second)
	says := countType[SayChannel](acts)
	if len(says) != 1 || says[0].Coalesce != statusKey(chManaged) ||
		!strings.Contains(says[0].HTML, "<td><b>Alice</b></td><td>剩余 9分钟<br>") ||
		!strings.Contains(says[0].HTML, `<td colspan="2">Bob</td>`) {
		t.Fatalf("status announcement = %v", says)
	}

	// A queue change resets the interval.
	h.advance(30 * time.Second)
	h.join(3, "Carol", chManaged)
	if says := countType[SayChannel](h.advance(59 * time.Second)); len(says) != 0 {
		t.Fatalf("status repeated right after a change: %v", says)
	}
}

func TestQueueChangeIsBroadcastAndJoinerTold(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	acts := h.sync()
	// Nobody gets a private join note at takeover; the broadcast lists them.
	if pm := privateTexts(acts, 2); len(pm) != 0 {
		t.Fatalf("join note at takeover: %v", pm)
	}

	acts = h.join(3, "Carol", chManaged)
	says := countType[SayChannel](acts)
	if len(says) != 1 || says[0].Coalesce != statusKey(chManaged) ||
		!strings.Contains(says[0].HTML, "排队（2 人）") || !strings.Contains(says[0].HTML, `<td colspan="2">Carol</td>`) {
		t.Fatalf("queue broadcast = %v", says)
	}
	if pm := privateTexts(acts, 3); len(pm) != 1 || !strings.Contains(pm[0], "你已加入麦序队列，当前排在第 <b>2</b> 位") ||
		!strings.Contains(pm[0], "You joined the queue as number <b>2</b>") {
		t.Fatalf("join note = %v", pm)
	}

	// Leaving the queue is broadcast too.
	acts = h.join(2, "Bob", chOther)
	if says := channelTexts(acts); len(says) != 1 || strings.Contains(says[0], "Bob") || !strings.Contains(says[0], "排队（1 人）") {
		t.Fatalf("broadcast after leaving = %v", says)
	}

	// Nothing changed: nothing is sent.
	if acts := h.change(h.users[3], ActorNone); len(countType[SayChannel](acts)) != 0 {
		t.Fatalf("broadcast without a change: %v", acts)
	}
}

func TestJoiningEmptyChannelStartsTurnWithoutJoinNote(t *testing.T) {
	h := newHarness(t)
	h.sync()
	acts := h.join(1, "Alice", chManaged)
	pm := privateTexts(acts, 1)
	if len(pm) != 1 || !strings.Contains(pm[0], "1分钟</b></font>。时间到后将转至「休息区」") {
		t.Fatalf("private messages = %v", pm)
	}
}

func TestEventNotesAndStatusShareOneMessage(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	acts := h.join(1, "Alice", chOther)
	says := countType[SayChannel](acts)
	if len(says) != 1 {
		t.Fatalf("want one broadcast, got %v", says)
	}
	html := says[0].HTML
	for _, want := range []string{
		"<b>Alice</b> 已离开频道", "<b>Alice</b> left the channel",
		"轮到 <b>Bob</b> 发言", "<b>Bob</b>'s turn to speak: 1m.",
		"<td><b>Bob</b></td>", `<td colspan="2">Carol</td>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("broadcast lacks %q: %s", want, html)
		}
	}
	if says[0].Coalesce != "" || says[0].Replaces != statusKey(chManaged) {
		t.Fatalf("coalesce = %q, replaces = %q", says[0].Coalesce, says[0].Replaces)
	}
	if !strings.HasPrefix(html, "<table") || !strings.HasSuffix(html, "</table>") || !strings.Contains(html, "麦序 Mic Queue · 会议室") {
		t.Fatalf("not a card: %s", html)
	}
}

func TestCCChannelsGetBroadcasts(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 60s/下麦转20/CC 30,99,10]"
	h.addInitial(1, "Alice", chManaged)
	acts := h.sync()
	says := countType[SayChannel](acts)
	if len(says) != 1 || len(says[0].CC) != 1 || says[0].CC[0] != chOther {
		t.Fatalf("CC = %v", says)
	}
	if !strings.Contains(says[0].HTML, "频道消息同时发送到「大厅」") || !strings.Contains(says[0].HTML, "CC 频道 99 不存在") {
		t.Fatalf("takeover = %s", says[0].HTML)
	}
	// Private messages are not copied.
	for _, a := range countType[SayUser](acts) {
		if a.Session != 1 {
			t.Fatalf("private message to %d", a.Session)
		}
	}

	// Dropping CC takes effect immediately.
	h.rename(chManaged, "会议室 [麦序/发言时间: 60s/下麦转20]")
	for _, s := range countType[SayChannel](h.join(2, "Bob", chManaged)) {
		if len(s.CC) > 0 {
			t.Fatalf("CC after removal: %v", s)
		}
	}

	// The stop notice still reaches the CC channels.
	h.rename(chManaged, "会议室 [麦序/发言时间: 60s/下麦转20/CC 30]")
	acts = h.rename(chManaged, "会议室")
	if says := countType[SayChannel](acts); len(says) != 1 || len(says[0].CC) != 1 || !strings.Contains(says[0].HTML, "麦序模式已关闭") {
		t.Fatalf("stop notice = %v", says)
	}
}

// addRegistered adds a registered user who is already connected when the
// bot syncs.
func (h *harness) addRegistered(session, userID uint32, name string, channel uint32) {
	h.users[session] = UserInfo{Session: session, UserID: userID, Name: name, ChannelID: channel, Suppressed: channel == chManaged}
}

// connectAgain simulates a registered user connecting a second time straight
// into a channel while the old session is still there.
func (h *harness) connectAgain(session, userID uint32, name string, channel uint32) []Action {
	return h.change(UserInfo{Session: session, UserID: userID, Name: name, ChannelID: channel, Suppressed: channel == chManaged}, ActorNone)
}

func TestSpeakerGhostSessionDoesNotEndTurn(t *testing.T) {
	h := newHarness(t)
	h.addRegistered(1, 5, "Alice", chManaged)
	h.addRegistered(2, 6, "Bob", chManaged)
	h.sync()
	h.advance(10 * time.Second)

	// Alice reconnects; the server has not dropped the old session yet.
	acts := h.connectAgain(11, 5, "Alice", chManaged)
	h.wantSpeaker("Alice")
	h.wantQueue("Bob")
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 11 {
		t.Fatalf("unsuppress = %v", u)
	}
	if len(countType[SayChannel](acts)) != 0 || len(countType[SayUser](acts)) != 0 {
		t.Fatalf("a second session was announced: %s", joinText(acts))
	}

	// The ghost goes away: nothing happens, no offline wait, no bonus.
	acts = h.leave(1)
	if hasText(acts, "已离线") || hasText(acts, "重新连接") {
		t.Fatalf("ghost removal treated as a disconnect: %s", joinText(acts))
	}
	if h.remaining() != 50*time.Second {
		t.Fatalf("remaining = %v", h.remaining())
	}

	// Reminders go to the live session; the turn ends on time.
	acts = h.advance(20 * time.Second)
	if pm := privateTexts(acts, 11); len(pm) != 1 {
		t.Fatalf("reminders to the new session = %v", pm)
	}
	acts = h.advance(30 * time.Second)
	if m := countType[Move](acts); len(m) != 1 || m[0].Session != 11 {
		t.Fatalf("moves = %v", m)
	}
	h.wantSpeaker("Bob")
}

func TestDuplicateSessionsAllMovedAtEndOfTurn(t *testing.T) {
	h := newHarness(t)
	h.addRegistered(1, 5, "Alice", chManaged)
	h.addRegistered(2, 6, "Bob", chManaged)
	h.sync()
	h.connectAgain(11, 5, "Alice", chManaged)
	if h.users[1].Suppressed || h.users[11].Suppressed {
		t.Fatal("both of the speaker's sessions should be able to talk")
	}

	acts := h.advance(60 * time.Second)
	var moved []uint32
	for _, m := range countType[Move](acts) {
		moved = append(moved, m.Session)
	}
	if len(moved) != 2 || h.users[1].ChannelID != chTarget || h.users[11].ChannelID != chTarget {
		t.Fatalf("moved = %v", moved)
	}
	if pm := countType[SayUser](acts); len(pm) == 0 || privateTexts(acts, 1) != nil {
		t.Fatalf("private messages should go to the newest session only: %v", pm)
	}
	h.wantSpeaker("Bob")
	if len(countType[SetMute](h.advance(10*time.Second))) != 0 {
		t.Fatal("muted after a confirmed move")
	}
}

func TestDuplicateSessionsMutedWhenMoveFails(t *testing.T) {
	h := newHarness(t)
	h.noMoves = true
	h.addRegistered(1, 5, "Alice", chManaged)
	h.addRegistered(2, 6, "Bob", chManaged)
	h.sync()
	h.connectAgain(11, 5, "Alice", chManaged)
	h.advance(60 * time.Second)
	acts := h.advance(5 * time.Second)
	if m := countType[SetMute](acts); len(m) != 2 {
		t.Fatalf("mutes = %v", m)
	}
	// Leaving with both sessions lifts both mutes.
	h.join(1, "Alice", chOther)
	acts = h.join(11, "Alice", chOther)
	if m := countType[SetMute](acts); len(m) != 2 {
		t.Fatalf("unmutes = %v", m)
	}
}

func TestQueuedUserWithTwoSessionsQueuesOnce(t *testing.T) {
	h := newHarness(t)
	h.addRegistered(1, 5, "Alice", chManaged)
	h.addRegistered(2, 6, "Bob", chManaged)
	h.addRegistered(3, 7, "Carol", chManaged)
	h.sync()

	acts := h.connectAgain(12, 6, "Bob", chManaged)
	h.wantQueue("Bob", "Carol")
	if len(countType[SayChannel](acts)) != 0 || len(countType[SayUser](acts)) != 0 {
		t.Fatalf("second session announced: %s", joinText(acts))
	}
	h.leave(2)
	h.wantQueue("Bob", "Carol")
	acts = h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 12 {
		t.Fatalf("unsuppress = %v", u)
	}
	if hasText(acts, "断线") {
		t.Fatalf("Bob shown offline: %s", joinText(acts))
	}
}

func TestSpeakerSecondSessionLeavingKeepsTurn(t *testing.T) {
	h := newHarness(t)
	h.addRegistered(1, 5, "Alice", chManaged)
	h.addRegistered(2, 6, "Bob", chManaged)
	h.sync()
	h.connectAgain(11, 5, "Alice", chManaged)
	acts := h.join(11, "Alice", chOther)
	h.wantSpeaker("Alice")
	if hasText(acts, "已离开频道") {
		t.Fatalf("turn ended while a session is still there: %s", joinText(acts))
	}
	acts = h.join(1, "Alice", chOther)
	if !hasText(acts, "<b>Alice</b> 已离开频道") {
		t.Fatalf("turn did not end when the last session left: %s", joinText(acts))
	}
	h.wantSpeaker("Bob")
}

func TestIdleChannelIsQuiet(t *testing.T) {
	h := newHarness(t)
	h.sync()
	acts := h.advance(5 * time.Minute)
	if len(countType[SayChannel](acts)) != 0 {
		t.Fatalf("idle channel spoke: %v", acts)
	}
}

func TestMarkerRemovalStopsManagement(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()

	acts := h.rename(chManaged, "会议室")
	if !hasText(acts, "麦序模式已关闭") {
		t.Fatal("missing stop message")
	}
	acts = h.advance(5 * time.Minute)
	if len(countType[Move](acts)) != 0 || len(countType[SayChannel](acts)) != 0 {
		t.Fatalf("still managing: %v", acts)
	}
	h.join(3, "Carol", chManaged)
	if h.speaker() != "" {
		t.Fatal("still tracking")
	}
}

func TestInvalidMarkerKeepsPreviousSettings(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	acts := h.rename(chManaged, "会议室 [麦序/发言时间: abc/下麦转20]")
	if !hasText(acts, "无法识别的发言时间") {
		t.Fatalf("missing invalid-marker warning: %v", acts)
	}
	h.wantSpeaker("Alice")
	acts = h.advance(60 * time.Second)
	if len(countType[Move](acts)) != 1 {
		t.Fatal("stopped managing on a typo")
	}
}

func TestDurationChangeAppliesNextTurn(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.advance(10 * time.Second)
	h.rename(chManaged, "会议室 [麦序/发言时间: 2分钟/下麦转30]")
	if h.remaining() != 50*time.Second {
		t.Fatalf("remaining changed mid-turn: %v", h.remaining())
	}
	acts := h.advance(50 * time.Second)
	moves := countType[Move](acts)
	if len(moves) != 1 || moves[0].ChannelID != chOther {
		t.Fatalf("target change not applied immediately: %v", moves)
	}
	if h.remaining() != 2*time.Minute {
		t.Fatalf("next turn duration = %v", h.remaining())
	}
}

func TestRegistrationKeepsPlace(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	u := h.users[2]
	u.UserID = 42
	h.change(u, ActorOther)
	h.wantQueue("Bob", "Carol")
}

func TestRestoreAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.addInitial(4, "Dave", chManaged)
	h.sync()
	h.advance(20 * time.Second)
	// Carol leaves and rejoins, moving her behind Dave.
	h.join(3, "Carol", chOther)
	h.join(3, "Carol", chManaged)
	h.wantQueue("Bob", "Dave", "Carol")

	data, err := h.m.MarshalState()
	if err != nil {
		t.Fatal(err)
	}

	// Restart: new manager, same server. Alice is still unsuppressed.
	h2 := newHarness(t)
	h2.users = h.users
	h2.now = h.now.Add(time.Minute)
	if err := h2.m.RestoreState(data); err != nil {
		t.Fatal(err)
	}
	acts := h2.sync()
	h2.wantSpeaker("Alice")
	h2.wantQueue("Bob", "Dave", "Carol")
	if h2.remaining() != 40*time.Second {
		t.Fatalf("remaining after restart = %v", h2.remaining())
	}
	if hasText(acts, "本频道已启用麦序模式") || hasText(acts, "轮到") {
		t.Fatalf("restart restarted the turn: %v", acts)
	}
	if !hasText(acts, "麦序继续") {
		t.Fatal("missing resume message")
	}
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatal("unsuppressed an already unsuppressed speaker")
	}
}

func TestRestoreWhenSpeakerGoneAndSuppressed(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	data, _ := h.m.MarshalState()

	// Alice left while the bot was down; Bob was re-suppressed.
	h2 := newHarness(t)
	h2.addInitial(2, "Bob", chManaged)
	h2.m.RestoreState(data)
	acts := h2.sync()
	h2.wantSpeaker("Alice") // she may still be reconnecting
	if !hasText(acts, "<b>Alice</b> 已离线，麦序暂停") {
		t.Fatalf("missing waiting message: %v", acts)
	}
	acts = h2.advance(60 * time.Second)
	h2.wantSpeaker("Bob")
	if !hasText(acts, "<b>Alice</b> 离线超过") {
		t.Fatalf("missing left message: %v", acts)
	}
}

func TestRestoreResuppressedSpeaker(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	data, _ := h.m.MarshalState()

	h2 := newHarness(t)
	h2.addInitial(1, "Alice", chManaged) // suppressed again
	h2.m.RestoreState(data)
	acts := h2.sync()
	h2.wantSpeaker("Alice")
	if len(countType[Unsuppress](acts)) != 1 {
		t.Fatalf("want unsuppress for restored speaker: %v", acts)
	}
}

func TestPersistRequested(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	acts := h.advance(time.Second)
	if len(countType[Persist](acts)) != 1 {
		t.Fatal("state change not persisted")
	}
	acts = h.advance(4 * time.Second)
	if len(countType[Persist](acts)) != 0 {
		t.Fatal("persisted before the interval")
	}
	acts = h.advance(time.Second)
	if len(countType[Persist](acts)) != 1 {
		t.Fatalf("periodic persist count = %d", len(countType[Persist](acts)))
	}
}

func TestChannelRemoved(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	h.join(1, "Alice", chRoot)
	delete(h.chans, chManaged)
	h.run(h.m.Handle(ChannelGone{ID: chManaged}, h.now))
	h.m.mu.Lock()
	_, ok := h.m.managed[chManaged]
	h.m.mu.Unlock()
	if ok {
		t.Fatal("removed channel still managed")
	}
}

func TestOfflineSpeakerBackWithinWaitGetsBonus(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.advance(10 * time.Second)

	h.leave(1)
	acts := h.advance(30 * time.Second)
	if h.remaining() != 50*time.Second {
		t.Fatalf("clock ran while offline: %v", h.remaining())
	}
	if len(countType[Unsuppress](acts)) != 0 {
		t.Fatal("queue not held")
	}

	acts = h.join(11, "Alice", chManaged)
	h.wantSpeaker("Alice")
	if h.remaining() != 80*time.Second {
		t.Fatalf("remaining after rejoin = %v, want 50s + 30s bonus", h.remaining())
	}
	if u := countType[Unsuppress](acts); len(u) != 1 || u[0].Session != 11 {
		t.Fatalf("unsuppress = %v", u)
	}
	if !hasText(acts, "<b>Alice</b> 已重新连接，计时继续，补偿 30秒（剩余 1分20秒）") {
		t.Fatalf("missing rejoin message: %s", joinText(acts))
	}
	if acts = h.advance(79 * time.Second); len(countType[Move](acts)) != 0 {
		t.Fatal("turn ended early")
	}
	if acts = h.advance(time.Second); len(countType[Move](acts)) != 1 {
		t.Fatal("turn did not end")
	}
}

func TestOfflineSpeakerCutsInWithinWindow(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.advance(10 * time.Second)

	h.leave(1)
	acts := h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	if !hasText(acts, "如果 10分钟 内重新连接，将排在当前发言人之后继续发言") {
		t.Fatalf("missing drop message: %s", joinText(acts))
	}
	h.join(4, "Dave", chManaged)
	h.wantQueue("Carol", "Dave")

	h.advance(30 * time.Second)
	acts = h.join(11, "Alice", chManaged)
	h.wantQueue("Alice", "Carol", "Dave")
	if !hasText(acts, "<b>Alice</b> 已重新连接，排在当前发言人之后继续发言") {
		t.Fatalf("missing cut-in message: %s", joinText(acts))
	}

	acts = h.advance(30 * time.Second) // Bob's turn ends
	h.wantSpeaker("Alice")
	if h.remaining() != 80*time.Second {
		t.Fatalf("resumed remaining = %v, want 50s left + 30s bonus", h.remaining())
	}
	if !hasText(acts, "轮到 <b>Alice</b> 继续发言") {
		t.Fatalf("missing resume turn message: %s", joinText(acts))
	}
	h.advance(80 * time.Second)
	h.wantSpeaker("Carol")
	if h.remaining() != 60*time.Second {
		t.Fatalf("normal turn length changed: %v", h.remaining())
	}
}

func TestOfflineSpeakerCutsInWhenIdle(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.sync()
	h.advance(20 * time.Second)
	h.leave(1)
	h.advance(60 * time.Second)
	h.wantSpeaker("")

	h.advance(5 * time.Minute)
	acts := h.join(11, "Alice", chManaged)
	h.wantSpeaker("Alice")
	if h.remaining() != 70*time.Second {
		t.Fatalf("remaining = %v, want 40s + 30s", h.remaining())
	}
	if u := countType[Unsuppress](acts); len(u) != 1 {
		t.Fatalf("unsuppress = %v", u)
	}
}

func TestOfflineSpeakerAfterWindowQueuesNormally(t *testing.T) {
	h := newHarness(t)
	h.chans[chManaged] = "会议室 [麦序/发言时间: 20分钟/下麦转20]"
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.leave(1)
	h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	h.advance(10 * time.Minute)
	h.join(11, "Alice", chManaged)
	h.wantQueue("Carol", "Alice")
}

func TestReturningSpeakersKeepOrder(t *testing.T) {
	cs := newChannelState(1)
	cs.queue = []member{{Key: "c", Name: "C"}, {Key: "d", Name: "D"}}
	cs.insertResumed(member{Key: "a", Name: "A", Resume: time.Second})
	cs.insertResumed(member{Key: "b", Name: "B", Resume: time.Second})
	var got []string
	for _, q := range cs.queue {
		got = append(got, q.Key)
	}
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("queue = %v", got)
	}
}

func TestDroppedSpeakerSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.addInitial(3, "Carol", chManaged)
	h.sync()
	h.advance(10 * time.Second)
	h.leave(1)
	h.advance(60 * time.Second)
	h.wantSpeaker("Bob")
	data, err := h.m.MarshalState()
	if err != nil {
		t.Fatal(err)
	}

	h2 := newHarness(t)
	h2.users = h.users
	h2.now = h.now.Add(time.Minute)
	if err := h2.m.RestoreState(data); err != nil {
		t.Fatal(err)
	}
	h2.sync()
	h2.join(11, "Alice", chManaged)
	h2.wantQueue("Alice", "Carol")

	data, _ = h2.m.MarshalState()
	h3 := newHarness(t)
	h3.users = h2.users
	h3.now = h2.now
	h3.m.RestoreState(data)
	h3.sync()
	h3.wantQueue("Alice", "Carol")
	h3.advance(60 * time.Second)
	h3.wantSpeaker("Alice")
	if h3.remaining() != 80*time.Second {
		t.Fatalf("restored resume time = %v", h3.remaining())
	}
}

func TestMovingOutIsNotOffline(t *testing.T) {
	h := newHarness(t)
	h.addInitial(1, "Alice", chManaged)
	h.addInitial(2, "Bob", chManaged)
	h.sync()
	h.join(1, "Alice", chOther)
	h.wantSpeaker("Bob")
	h.join(1, "Alice", chManaged)
	h.wantQueue("Alice") // no cut-in: she left on purpose
}
