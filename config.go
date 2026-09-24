package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"micseqbot/internal/micseq"

	"layeh.com/gumble/gumble"
)

// Config is the bot's JSON configuration file.
type Config struct {
	// Server is "host" or "host:port".
	Server   string   `json:"server"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	Tokens   []string `json:"tokens"`

	// CertFile and KeyFile hold the bot's client certificate (PEM). They are
	// generated on first run if missing. Registering the bot requires one.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// Register self-registers the bot on first connect.
	Register bool `json:"register"`

	// ServerFingerprint pins the server certificate (SHA-256, hex). When empty
	// the fingerprint seen on first connect is stored in TrustFile and
	// required afterwards.
	ServerFingerprint string `json:"server_fingerprint"`
	TrustFile         string `json:"trust_file"`

	// HomeChannel is the channel ID the bot moves itself to; 0 keeps the
	// server default.
	HomeChannel uint32 `json:"home_channel"`

	StateFile string `json:"state_file"`

	QueueAnnounceInterval     Duration `json:"queue_announce_interval"`
	RemainingAnnounceInterval Duration `json:"remaining_announce_interval"`
	WarnBefore                Duration `json:"warn_before"`
	// RejoinGrace is how long disconnected queued users keep their place,
	// e.g. while everyone reconnects after a server restart.
	RejoinGrace Duration `json:"rejoin_grace"`
	// SpeakerOfflineWait is how long the queue is held when the speaker goes
	// offline; RejoinBonus is extra time for a speaker who comes back; and a
	// speaker who comes back within RejoinWindow after losing the floor goes
	// right after the current speaker.
	SpeakerOfflineWait Duration `json:"speaker_offline_wait"`
	RejoinBonus        Duration `json:"rejoin_bonus"`
	RejoinWindow       Duration `json:"rejoin_window"`

	// MessageRate and MessageBurst must not exceed the server's
	// messagelimit/messageburst, otherwise the server silently drops text.
	MessageRate  float64 `json:"message_rate"`
	MessageBurst int     `json:"message_burst"`

	// IgnoreUsers are names never put into a queue (other bots, etc.).
	IgnoreUsers []string `json:"ignore_users"`
	PinyinSort  *bool    `json:"pinyin_sort"`
}

// Duration accepts a number of seconds or a string such as "60s", "1分钟"
// or "off"/"0" to disable.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a number of seconds or a string: %s", data)
	}
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "0", "off", "false":
		*d = 0
		return nil
	}
	v, err := micseq.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func defaultConfig() Config {
	return Config{
		Username:                  "麦序机器人",
		CertFile:                  "micseq-bot.crt",
		KeyFile:                   "micseq-bot.key",
		TrustFile:                 "server.fingerprint",
		StateFile:                 "micseq-state.json",
		QueueAnnounceInterval:     Duration(60 * time.Second),
		RemainingAnnounceInterval: Duration(60 * time.Second),
		WarnBefore:                Duration(30 * time.Second),
		RejoinGrace:               Duration(90 * time.Second),
		SpeakerOfflineWait:        Duration(time.Minute),
		RejoinBonus:               Duration(30 * time.Second),
		RejoinWindow:              Duration(10 * time.Minute),
		MessageRate:               1,
		MessageBurst:              5,
	}
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Server == "" {
		return errors.New("server is required")
	}
	if c.Username == "" {
		return errors.New("username is required")
	}
	if c.MessageRate <= 0 {
		return errors.New("message_rate must be positive")
	}
	if c.MessageBurst < 1 {
		c.MessageBurst = 1
	}
	c.ServerFingerprint = normalizeFingerprint(c.ServerFingerprint)
	return nil
}

// Address returns host:port, adding the default Mumble port if needed.
func (c *Config) Address() string {
	if _, _, err := net.SplitHostPort(c.Server); err == nil {
		return c.Server
	}
	return net.JoinHostPort(strings.Trim(c.Server, "[]"), strconv.Itoa(gumble.DefaultPort))
}

func (c *Config) managerConfig() micseq.Config {
	mc := micseq.DefaultConfig()
	mc.QueueInterval = time.Duration(c.QueueAnnounceInterval)
	mc.RemainingInterval = time.Duration(c.RemainingAnnounceInterval)
	mc.WarnBefore = time.Duration(c.WarnBefore)
	mc.RejoinGrace = time.Duration(c.RejoinGrace)
	mc.SpeakerOfflineWait = time.Duration(c.SpeakerOfflineWait)
	mc.RejoinBonus = time.Duration(c.RejoinBonus)
	mc.RejoinWindow = time.Duration(c.RejoinWindow)
	mc.Less = micseq.NameLess(c.PinyinSort == nil || *c.PinyinSort)
	return mc
}
