//go:build e2e

// End-to-end tests against a real Mumble server. Start one with, e.g.:
//
//	docker run -d --name micseq-e2e -p 64799:64738 \
//	  -e MUMBLE_SUPERUSER_PASSWORD=e2epass -e MUMBLE_CONFIG_AUTOBANATTEMPTS=0 \
//	  mumblevoip/mumble-server
//	MICSEQ_E2E_ADDR=127.0.0.1:64799 MICSEQ_E2E_SUPERUSER_PASSWORD=e2epass \
//	  MICSEQ_E2E_CONTAINER=micseq-e2e go test -tags e2e -v .
//
// MICSEQ_E2E_CONTAINER is only needed by TestServerRestart and
// TestServerOutages, which stop, kill, pause and restart the container.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"layeh.com/gumble/gumble"
	"layeh.com/gumble/gumbleutil"
)

type e2eClient struct {
	t     *testing.T
	name  string
	c     *gumble.Client
	mu    sync.Mutex
	texts []string
}

func tryDialE2E(t *testing.T, addr, name, password string) (*e2eClient, error) {
	ec := &e2eClient{t: t, name: name}
	cfg := gumble.NewConfig()
	cfg.Username = name
	cfg.Password = password
	cfg.Attach(gumbleutil.Listener{
		TextMessage: func(e *gumble.TextMessageEvent) {
			ec.mu.Lock()
			ec.texts = append(ec.texts, e.Message)
			ec.mu.Unlock()
		},
	})
	c, err := gumble.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, addr, cfg, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	ec.c = c
	t.Cleanup(func() { c.Disconnect() })
	return ec, nil
}

func dialE2E(t *testing.T, addr, name, password string) *e2eClient {
	t.Helper()
	ec, err := tryDialE2E(t, addr, name, password)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	return ec
}

// dialRetry keeps trying while the server is (re)starting.
func dialRetry(t *testing.T, addr, name, password string, timeout time.Duration) *e2eClient {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ec, err := tryDialE2E(t, addr, name, password)
		if err == nil {
			return ec
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", name, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (ec *e2eClient) got(substr string) bool {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, s := range ec.texts {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func (ec *e2eClient) dump() string {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return strings.Join(ec.texts, "\n")
}

// view reads a user as seen by this client.
func (ec *e2eClient) view(name string) (u gumble.User, ok bool) {
	ec.c.Do(func() {
		if p := ec.c.Users.Find(name); p != nil {
			u, ok = *p, true
		}
	})
	return
}

func (ec *e2eClient) channelID(name string) (id uint32, ok bool) {
	ec.c.Do(func() {
		for _, ch := range ec.c.Channels {
			if ch.Name == name {
				id, ok = ch.ID, true
			}
		}
	})
	return
}

func (ec *e2eClient) enter(channelID uint32) {
	ec.c.Do(func() { ec.c.Self.Move(ec.c.Channels[channelID]) })
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func holdFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("condition broke early: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// e2eEnv is a server with a room (Speak denied, bot allowed to manage it),
// a target channel and a running, registered bot.
type e2eEnv struct {
	t                    *testing.T
	addr, password       string
	suffix               string
	admin                *e2eClient
	roomName, targetName string
	roomID, targetID     uint32
	cfg                  Config
	botID                uint32
	stopBot              func()
	botDone              chan struct{}
}

func setupE2E(t *testing.T) *e2eEnv {
	addr := os.Getenv("MICSEQ_E2E_ADDR")
	password := os.Getenv("MICSEQ_E2E_SUPERUSER_PASSWORD")
	if addr == "" || password == "" {
		t.Skip("MICSEQ_E2E_ADDR and MICSEQ_E2E_SUPERUSER_PASSWORD not set")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixMilli()%1000000)
	env := &e2eEnv{
		t: t, addr: addr, password: password, suffix: suffix,
		roomName: "Room" + suffix, targetName: "Target" + suffix,
	}
	env.admin = dialE2E(t, addr, "SuperUser", password)
	env.admin.c.Do(func() {
		env.admin.c.Channels[0].Add(env.roomName, false)
		env.admin.c.Channels[0].Add(env.targetName, false)
	})
	waitFor(t, 5*time.Second, "channels created", func() bool {
		var ok1, ok2 bool
		env.roomID, ok1 = env.admin.channelID(env.roomName)
		env.targetID, ok2 = env.admin.channelID(env.targetName)
		return ok1 && ok2
	})

	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Server = addr
	cfg.Username = "MicBot" + suffix
	cfg.CertFile = filepath.Join(dir, "bot.crt")
	cfg.KeyFile = filepath.Join(dir, "bot.key")
	cfg.TrustFile = filepath.Join(dir, "server.fingerprint")
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.QueueAnnounceInterval = Duration(6 * time.Second)
	cfg.RemainingAnnounceInterval = Duration(4 * time.Second)
	cfg.WarnBefore = Duration(3 * time.Second)
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	env.cfg = cfg
	env.startBot()
	t.Cleanup(func() { env.stopBot() })

	// Register the bot and give it MuteDeafen/Move/TextMessage in the room,
	// while denying Speak to everyone there.
	waitFor(t, 10*time.Second, "bot connected", func() bool { _, ok := env.admin.view(cfg.Username); return ok })
	env.admin.c.Do(func() { env.admin.c.Users.Find(cfg.Username).Register() })
	waitFor(t, 5*time.Second, "bot registered", func() bool {
		u, _ := env.admin.view(cfg.Username)
		env.botID = u.UserID
		return env.botID > 0
	})
	env.writeRoomACL()
	time.Sleep(500 * time.Millisecond)
	return env
}

func (env *e2eEnv) startBot() {
	bot := NewBot(&env.cfg)
	if err := bot.LoadState(); err != nil {
		env.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bot.Run(ctx); close(done) }()
	env.botDone = done
	env.stopBot = func() { cancel(); <-done }
}

// botAlive fails the test if the bot's Run loop has exited.
func (env *e2eEnv) botAlive() {
	env.t.Helper()
	select {
	case <-env.botDone:
		env.t.Fatal("bot stopped running")
	default:
	}
}

func (env *e2eEnv) writeRoomACL() {
	admin := env.admin
	admin.c.Do(func() {
		admin.c.Send(&gumble.ACL{
			Channel:  admin.c.Channels[env.roomID],
			Inherits: true,
			Rules: []*gumble.ACLRule{
				{AppliesCurrent: true, AppliesChildren: true, Group: &gumble.ACLGroup{Name: "all"}, Denied: gumble.PermissionSpeak},
				{AppliesCurrent: true, AppliesChildren: true, User: &gumble.ACLUser{UserID: env.botID},
					Granted: gumble.PermissionMuteDeafen | gumble.PermissionMove | gumble.PermissionTextMessage},
			},
		})
	})
}

func (env *e2eEnv) setMarker(duration string) {
	env.admin.c.Do(func() {
		env.admin.c.Channels[env.roomID].SetName(fmt.Sprintf("%s [麦序/发言时间: %s/下麦转%d]", env.roomName, duration, env.targetID))
	})
}

func (env *e2eEnv) in(name string, channelID uint32) func() bool {
	return func() bool {
		u, ok := env.admin.view(name)
		return ok && u.Channel != nil && u.Channel.ID == channelID
	}
}

func (env *e2eEnv) suppressed(name string) bool {
	u, _ := env.admin.view(name)
	return u.Suppressed
}

// joinRoom connects a user and moves them into the room.
func (env *e2eEnv) joinRoom(name string, timeout time.Duration) *e2eClient {
	env.t.Helper()
	ec := dialRetry(env.t, env.addr, name+env.suffix, "", timeout)
	ec.enter(env.roomID)
	waitFor(env.t, 5*time.Second, name+" in room", env.in(ec.name, env.roomID))
	return ec
}

func TestEndToEnd(t *testing.T) {
	env := setupE2E(t)
	admin, cfg := env.admin, env.cfg
	in, suppressed := env.in, env.suppressed

	carol := env.joinRoom("Carol", 5*time.Second)
	alice := env.joinRoom("Alice", 5*time.Second)
	bob := env.joinRoom("Bob", 5*time.Second)
	aliceN, bobN, carolN := alice.name, bob.name, carol.name
	for _, n := range []string{aliceN, bobN, carolN} {
		if !suppressed(n) {
			t.Fatalf("%s not suppressed; ACL not effective", n)
		}
	}

	env.setMarker("8s")

	// Join order was observed: Carol, Alice, Bob.
	waitFor(t, 5*time.Second, "Carol unsuppressed", func() bool { return !suppressed(carolN) })
	if suppressed(carolN) || !suppressed(aliceN) || !suppressed(bobN) {
		t.Fatal("wrong speaker")
	}
	waitFor(t, 5*time.Second, "takeover message", func() bool {
		return carol.got("本频道已启用麦序模式") && carol.got("轮到 <b>"+carolN+"</b>")
	})

	// Carol leaves early; Alice gets the floor.
	carol.enter(0)
	waitFor(t, 5*time.Second, "Alice unsuppressed after Carol left", func() bool { return !suppressed(aliceN) })
	waitFor(t, 5*time.Second, "left-early message", func() bool { return alice.got("已离开频道，提前结束发言") })

	// Bob gets a private heads-up, Alice is moved to the target when time is up.
	waitFor(t, 10*time.Second, "Bob private warning", func() bool { return bob.got("下一位发言者") })
	waitFor(t, 10*time.Second, "Alice moved to target", in(aliceN, env.targetID))
	waitFor(t, 5*time.Second, "Bob unsuppressed", func() bool { return !suppressed(bobN) })

	// Admin mutes Bob: the timer pauses.
	time.Sleep(2 * time.Second)
	admin.c.Do(func() { admin.c.Users.Find(bobN).SetMuted(true) })
	waitFor(t, 5*time.Second, "pause message", func() bool { return bob.got("计时暂停") })
	holdFor(t, 10*time.Second, "Bob stays while muted", in(bobN, env.roomID))

	// Restart the bot mid-turn: it resumes from the state file instead of
	// starting a new turn.
	env.stopBot()
	waitFor(t, 5*time.Second, "bot gone", func() bool { _, ok := admin.view(cfg.Username); return !ok })
	env.startBot()
	waitFor(t, 10*time.Second, "resume after restart", func() bool { return bob.got("机器人已重新连接，麦序继续") })
	holdFor(t, 3*time.Second, "Bob still speaking after restart", func() bool { return in(bobN, env.roomID)() && !suppressed(bobN) })
	if strings.Count(bob.dump(), "轮到 <b>"+bobN+"</b>") != 1 {
		t.Fatal("restart started a new turn")
	}

	// Dave joins and an admin unsuppresses him: he leaves the queue for good.
	dave := env.joinRoom("Dave", 5*time.Second)
	waitFor(t, 5*time.Second, "Dave suppressed", func() bool { return suppressed(dave.name) })
	admin.c.Do(func() { admin.c.Users.Find(dave.name).SetSuppressed(false) })
	waitFor(t, 5*time.Second, "exempt message", func() bool { return dave.got("已由管理员直接授予发言权限") })

	// An ACL edit re-suppresses everyone; the bot unsuppresses Bob again.
	env.writeRoomACL()
	time.Sleep(time.Second)
	waitFor(t, 8*time.Second, "Bob unsuppressed again after ACL edit", func() bool { return !suppressed(bobN) })

	// Unmute: the timer resumes and Bob is eventually moved.
	admin.c.Do(func() { admin.c.Users.Find(bobN).SetMuted(false) })
	waitFor(t, 5*time.Second, "resume message", func() bool { return bob.got("计时继续") })
	waitFor(t, 12*time.Second, "Bob moved to target", in(bobN, env.targetID))

	// Dave was exempted, so nobody is left in the queue and he is not moved.
	waitFor(t, 5*time.Second, "queue empty message", func() bool { return dave.got("当前无人排队") })
	holdFor(t, 3*time.Second, "Dave stays in room", in(dave.name, env.roomID))

	if _, err := os.Stat(cfg.StateFile); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Removing the marker stops management.
	admin.c.Do(func() { admin.c.Channels[env.roomID].SetName(env.roomName) })
	waitFor(t, 5*time.Second, "stop message", func() bool { return dave.got("麦序模式已关闭") })

	t.Logf("messages seen by Dave:\n%s", dave.dump())
	t.Logf("messages seen by Bob:\n%s", bob.dump())
}

func TestServerRestart(t *testing.T) {
	container := os.Getenv("MICSEQ_E2E_CONTAINER")
	if container == "" {
		t.Skip("MICSEQ_E2E_CONTAINER not set")
	}
	env := setupE2E(t)

	alice := env.joinRoom("Alice", 5*time.Second)
	env.joinRoom("Bob", 5*time.Second)
	env.joinRoom("Carol", 5*time.Second)
	aliceN, bobN, carolN := alice.name, "Bob"+env.suffix, "Carol"+env.suffix
	env.setMarker("20s")
	waitFor(t, 5*time.Second, "Alice speaking", func() bool { return !env.suppressed(aliceN) })
	time.Sleep(3 * time.Second)

	t.Log("restarting the Mumble server")
	if out, err := exec.Command("docker", "restart", container).CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v\n%s", err, out)
	}

	// Users come back in a different order: Carol, Bob, and Alice last.
	env.admin = dialRetry(t, env.addr, "SuperUser", env.password, 60*time.Second)
	carol := env.joinRoom("Carol", 30*time.Second)
	bob := env.joinRoom("Bob", 30*time.Second)
	waitFor(t, 60*time.Second, "bot reconnected", func() bool { _, ok := env.admin.view(env.cfg.Username); return ok })
	holdFor(t, 4*time.Second, "floor held for the absent speaker", func() bool {
		return env.suppressed(bobN) && env.suppressed(carolN)
	})

	alice = env.joinRoom("Alice", 30*time.Second)
	waitFor(t, 5*time.Second, "Alice speaking again", func() bool { return !env.suppressed(aliceN) })
	waitFor(t, 5*time.Second, "rejoin message", func() bool { return alice.got("已重新连接，计时继续，补偿 30秒") })
	if !env.suppressed(bobN) || !env.suppressed(carolN) {
		t.Fatal("someone else has the floor")
	}

	// Alice finishes the rest of her turn, then Bob (who queued before
	// Carol) goes next even though Carol reconnected first.
	// 17s were left; the rejoin adds 30s.
	waitFor(t, 55*time.Second, "Alice moved to target", env.in(aliceN, env.targetID))
	waitFor(t, 5*time.Second, "Bob speaking", func() bool { return !env.suppressed(bobN) })
	if !env.suppressed(carolN) {
		t.Fatal("Carol jumped the queue")
	}
	t.Logf("messages seen by Carol:\n%s", carol.dump())
	t.Logf("messages seen by Bob:\n%s", bob.dump())
}

func dockerCmd(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// TestServerOutages takes the server away in several ways while a turn is
// running and checks that the bot survives, reconnects on its own, and
// resumes the same turn once people are back.
func TestServerOutages(t *testing.T) {
	container := os.Getenv("MICSEQ_E2E_CONTAINER")
	if container == "" {
		t.Skip("MICSEQ_E2E_CONTAINER not set")
	}
	env := setupE2E(t)
	aliceN, bobN := "Alice"+env.suffix, "Bob"+env.suffix
	env.joinRoom("Alice", 5*time.Second)
	env.joinRoom("Bob", 5*time.Second)
	env.setMarker("10分钟")
	waitFor(t, 5*time.Second, "Alice speaking", func() bool { return !env.suppressed(aliceN) })

	// rejoinAfter brings admin and users back and checks the turn resumed.
	rejoinAfter := func(what string) {
		t.Helper()
		env.botAlive()
		env.admin = dialRetry(t, env.addr, "SuperUser", env.password, 90*time.Second)
		// Clients of a frozen server linger as ghosts until the server times
		// them out, so rejoining may take a while.
		env.joinRoom("Bob", 90*time.Second)
		env.joinRoom("Alice", 90*time.Second)
		waitFor(t, maxBackoff+dialTimeout+30*time.Second, what+": bot reconnected", func() bool {
			_, ok := env.admin.view(env.cfg.Username)
			return ok
		})
		waitFor(t, 10*time.Second, what+": Alice speaking again", func() bool { return !env.suppressed(aliceN) })
		if !env.suppressed(bobN) {
			t.Fatalf("%s: Bob got the floor", what)
		}
		env.botAlive()
		t.Logf("%s: recovered", what)
	}

	// 1. Graceful stop, offline longer than the maximum reconnect backoff.
	t.Log("outage 1: docker stop, 45s offline")
	dockerCmd(t, "stop", container)
	time.Sleep(45 * time.Second)
	env.botAlive()
	dockerCmd(t, "start", container)
	rejoinAfter("stop/start")

	// 2. Hard kill: the process dies without a clean shutdown.
	t.Log("outage 2: docker kill")
	dockerCmd(t, "kill", container)
	time.Sleep(5 * time.Second)
	env.botAlive()
	dockerCmd(t, "start", container)
	rejoinAfter("kill/start")

	// 3. Frozen server: TCP stays open but nothing answers.
	t.Log("outage 3: docker pause, 40s")
	dockerCmd(t, "pause", container)
	time.Sleep(40 * time.Second)
	env.botAlive()
	dockerCmd(t, "unpause", container)
	rejoinAfter("pause/unpause")

	// 4. The bot starts while the server is offline.
	t.Log("outage 4: bot starts while the server is down")
	env.stopBot()
	dockerCmd(t, "stop", container)
	env.startBot()
	time.Sleep(15 * time.Second)
	env.botAlive()
	dockerCmd(t, "start", container)
	rejoinAfter("cold start")
}

// TestSpeakerOffline: the speaker disconnects; the queue waits a minute,
// then moves on; the speaker comes back within ten minutes and goes right
// after the current speaker.
func TestSpeakerOffline(t *testing.T) {
	env := setupE2E(t)
	alice := env.joinRoom("Alice", 5*time.Second)
	env.joinRoom("Bob", 5*time.Second)
	env.joinRoom("Carol", 5*time.Second)
	aliceN, bobN, carolN := alice.name, "Bob"+env.suffix, "Carol"+env.suffix
	env.setMarker("15s")
	waitFor(t, 5*time.Second, "Alice speaking", func() bool { return !env.suppressed(aliceN) })
	time.Sleep(3 * time.Second)

	alice.c.Disconnect()
	waitFor(t, 5*time.Second, "Alice gone", func() bool { _, ok := env.admin.view(aliceN); return !ok })
	holdFor(t, 55*time.Second, "queue held for the offline speaker", func() bool {
		return env.suppressed(bobN) && env.suppressed(carolN)
	})
	waitFor(t, 10*time.Second, "Bob speaking after the wait", func() bool { return !env.suppressed(bobN) })

	alice = env.joinRoom("Alice", 10*time.Second)
	waitFor(t, 5*time.Second, "cut-in message", func() bool { return alice.got("排在当前发言人之后继续发言") })

	waitFor(t, 20*time.Second, "Bob moved to target", env.in(bobN, env.targetID))
	waitFor(t, 5*time.Second, "Alice speaking before Carol", func() bool { return !env.suppressed(aliceN) })
	if !env.suppressed(carolN) {
		t.Fatal("Carol went before the returning speaker")
	}
	waitFor(t, 5*time.Second, "resume turn message", func() bool { return alice.got("继续发言（上次剩余时间加补偿）") })
	t.Logf("messages seen by Alice:\n%s", alice.dump())
}
