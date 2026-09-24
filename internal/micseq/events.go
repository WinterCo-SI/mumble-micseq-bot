package micseq

import "strconv"

// Actor describes who caused a user state change.
type Actor int

const (
	// ActorNone means the server changed the state on its own (for example a
	// suppress recalculation after an ACL edit) or the change was not caused
	// by anyone in particular.
	ActorNone Actor = iota
	// ActorSelf means the bot caused the change.
	ActorSelf
	// ActorOther means another user (usually an admin) caused the change.
	ActorOther
)

// UserInfo is a plain snapshot of a connected user.
type UserInfo struct {
	Session    uint32
	UserID     uint32 // 0 when unregistered (or SuperUser)
	Name       string
	ChannelID  uint32
	Muted      bool // server mute by an admin or the bot
	Deafened   bool // server deaf (implies muted)
	Suppressed bool
	// Ignored users are never queued: the bot itself, SuperUser and
	// configured ignore names.
	Ignored bool
}

// Key identifies a user across sessions and reconnects.
func (u UserInfo) Key() string {
	return UserKey(u.UserID, u.Name)
}

// UserKey returns "id:<UserID>" for registered users and "name:<Name>" otherwise.
func UserKey(userID uint32, name string) string {
	if userID > 0 {
		return "id:" + strconv.FormatUint(uint64(userID), 10)
	}
	return "name:" + name
}

// ChannelInfo is a plain snapshot of a channel.
type ChannelInfo struct {
	ID   uint32
	Name string
}

// Snapshot is the full server view taken right after (re)connecting.
type Snapshot struct {
	Channels []ChannelInfo
	Users    []UserInfo
}

// Event is something that happened on the server.
type Event interface{ isEvent() }

// UserChanged is sent whenever a user connects or any of their state changes.
type UserChanged struct {
	User  UserInfo
	Actor Actor
}

// UserGone is sent when a user disconnects.
type UserGone struct {
	Session uint32
}

// ChannelChanged is sent when a channel is created or renamed.
type ChannelChanged struct {
	Channel ChannelInfo
}

// ChannelGone is sent when a channel is removed.
type ChannelGone struct {
	ID uint32
}

func (UserChanged) isEvent()    {}
func (UserGone) isEvent()       {}
func (ChannelChanged) isEvent() {}
func (ChannelGone) isEvent()    {}

// Action is something the bot should do on the server.
type Action interface{ isAction() }

// Unsuppress lets a user speak in a channel whose ACL denies Speak.
type Unsuppress struct {
	Session uint32
}

// Move moves a user to another channel.
type Move struct {
	Session   uint32
	ChannelID uint32
}

// SetMute sets or clears a server mute.
type SetMute struct {
	Session uint32
	Muted   bool
}

// SayChannel sends an HTML text message to a channel and a copy to its CC
// channels. Pending messages with the same non-empty Coalesce key replace
// each other; pending messages whose Coalesce key equals Replaces are dropped.
type SayChannel struct {
	ChannelID uint32
	CC        []uint32
	HTML      string
	Coalesce  string
	Replaces  string
}

// SayUser sends a private HTML text message to a user.
type SayUser struct {
	Session uint32
	HTML    string
}

// Persist asks for the state file to be written.
type Persist struct{}

func (Unsuppress) isAction() {}
func (Move) isAction()       {}
func (SetMute) isAction()    {}
func (SayChannel) isAction() {}
func (SayUser) isAction()    {}
func (Persist) isAction()    {}
