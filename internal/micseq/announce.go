package micseq

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// Messages use the rich-text subset Mumble clients render (Qt's). Mumble has
// light and dark themes, so only the title row sets a background, together
// with its own text colour; everything else uses mid-tone text colours that
// read on either.
const (
	colorTitleBG = "#2f6fb0"
	colorTitleFG = "#ffffff"
	colorBorder  = "#8a99a8"
	colorMuted   = "#8a8a8a"
	colorGood    = "#3a9b4f"
	colorWarn    = "#d9822b"
	colorBad     = "#d64545"
)

// cardColumns is the column count of every card: position, name, state.
const cardColumns = 3

// maxListed caps how many queue entries are printed in one message.
const maxListed = 30

func b(name string) string {
	return "<b>" + html.EscapeString(name) + "</b>"
}

func quoteChan(name string) string {
	return "「" + html.EscapeString(name) + "」"
}

func quoteChanEN(name string) string {
	return "“" + html.EscapeString(name) + "”"
}

func colored(color, s string) string {
	return `<font color="` + color + `">` + s + `</font>`
}

func muted(s string) string {
	return colored(colorMuted, s)
}

func warn(s string) string {
	return colored(colorWarn, s)
}

// bi puts the English version of a sentence under the Chinese one.
func bi(zh, en string) string {
	return zh + "<br>" + muted(en)
}

// label puts the English version of a short label next to the Chinese one.
func label(zh, en string) string {
	return zh + " " + muted(en)
}

// countdown renders a remaining time, coloured by how close the deadline is.
func countdown(d time.Duration, format func(time.Duration) string) string {
	s := "<b>" + format(d) + "</b>"
	switch {
	case d <= 30*time.Second:
		return colored(colorBad, s)
	case d <= time.Minute:
		return colored(colorWarn, s)
	}
	return s
}

// card builds a message as a bordered table under a title row.
type card struct {
	sb strings.Builder
}

func newCard(channel string) *card {
	c := &card{}
	c.sb.WriteString(`<table border="1" cellspacing="0" cellpadding="4" style="border-style:solid;border-color:` + colorBorder + `;border-collapse:collapse">`)
	fmt.Fprintf(&c.sb, `<tr><th colspan="%d" align="left" bgcolor="%s">%s</th></tr>`,
		cardColumns, colorTitleBG, colored(colorTitleFG, "麦序 Mic Queue · "+html.EscapeString(channel)))
	return c
}

// line adds a full-width row.
func (c *card) line(s string) {
	fmt.Fprintf(&c.sb, `<tr><td colspan="%d">%s</td></tr>`, cardColumns, s)
}

// row adds a position/name/state row; an empty state spans the name. The
// position column has a fixed width, or Qt spreads a card widened by a long
// note evenly over all columns.
func (c *card) row(label, name, state string) {
	c.sb.WriteString(`<tr><td align="center" width="64">` + label + `</td>`)
	if state == "" {
		fmt.Fprintf(&c.sb, `<td colspan="%d">%s</td></tr>`, cardColumns-1, name)
		return
	}
	c.sb.WriteString(`<td>` + name + `</td><td>` + state + `</td></tr>`)
}

func (c *card) String() string {
	return c.sb.String() + "</table>"
}

func privateCard(channel, body string) string {
	c := newCard(channel)
	c.line(body)
	return c.String()
}

type statusLine struct {
	Name    string
	Offline bool
}

// speakerNote explains why a speaker's clock is stopped.
type speakerNote int

const (
	noteNone speakerNote = iota
	notePaused
	noteOffline
)

// status adds the speaker and the queue to a card.
func (c *card) status(speaker *member, remaining time.Duration, note speakerNote, queue []statusLine) {
	speaking := colored(colorGood, "<b>发言</b>") + "<br>" + muted("Speaking")
	if speaker == nil {
		c.row(speaking, muted("当前无人发言 · No one is speaking"), "")
	} else {
		zh := "剩余 " + FormatDuration(remaining)
		en := FormatDurationEN(remaining) + " left"
		switch note {
		case notePaused:
			zh, en = warn("已暂停")+"，"+zh, "paused, "+en
		case noteOffline:
			zh, en = warn("离线，等待重连")+"，"+zh, "offline, waiting, "+en
		}
		c.row(speaking, b(speaker.Name), bi(zh, en))
	}
	if len(queue) == 0 {
		c.line(muted("当前无人排队 · Queue is empty"))
		return
	}
	c.line(label(fmt.Sprintf("<b>排队（%d 人）</b>", len(queue)), fmt.Sprintf("Queue: %d", len(queue))))
	for i, q := range queue {
		if i == maxListed {
			c.line(muted(fmt.Sprintf("……另有 %d 人 · %d more", len(queue)-maxListed, len(queue)-maxListed)))
			break
		}
		state := ""
		if q.Offline {
			state = colored(colorBad, "断线，等待重连") + " " + muted("offline")
		}
		c.row(strconv.Itoa(i+1), html.EscapeString(q.Name), state)
	}
}

// Channel notes.

func msgTakeover(d time.Duration, target string, cc []string) string {
	zh := fmt.Sprintf("本频道已启用麦序模式：按进入顺序依次发言，每人 %s，结束后转至%s。", FormatDuration(d), quoteChan(target))
	en := fmt.Sprintf("Mic queue is on: members speak in the order they entered, %s each, then move to %s.", FormatDurationEN(d), quoteChanEN(target))
	if len(cc) > 0 {
		zhNames := make([]string, len(cc))
		enNames := make([]string, len(cc))
		for i, name := range cc {
			zhNames[i], enNames[i] = quoteChan(name), quoteChanEN(name)
		}
		zh += "频道消息同时发送到" + strings.Join(zhNames, "") + "。"
		en += " Channel messages are also sent to " + strings.Join(enNames, ", ") + "."
	}
	return bi(zh, en)
}

func msgResumeAfterRestart() string {
	return bi("机器人已重新连接，麦序继续。", "The bot has reconnected; the queue continues.")
}

func msgStopped() string {
	return bi("本频道的麦序模式已关闭。", "Mic queue is off in this channel.")
}

func msgTurnStart(name string, d time.Duration, resumed bool) string {
	if resumed {
		return bi(fmt.Sprintf("轮到 %s 继续发言（上次剩余时间加补偿），时长 %s。", b(name), FormatDuration(d)),
			fmt.Sprintf("%s continues speaking (time left plus compensation): %s.", b(name), FormatDurationEN(d)))
	}
	return bi(fmt.Sprintf("轮到 %s 发言，时长 %s。", b(name), FormatDuration(d)),
		fmt.Sprintf("%s's turn to speak: %s.", b(name), FormatDurationEN(d)))
}

func msgLeftEarly(name string) string {
	return bi(fmt.Sprintf("%s 已离开频道，提前结束发言。", b(name)),
		fmt.Sprintf("%s left the channel; their turn ended early.", b(name)))
}

func msgPaused(name string, remaining time.Duration) string {
	return bi(fmt.Sprintf("%s 已被管理员禁言，计时暂停（剩余 %s）。", b(name), FormatDuration(remaining)),
		fmt.Sprintf("%s was muted by an admin; the timer is paused (%s left).", b(name), FormatDurationEN(remaining)))
}

func msgResumed(name string, remaining time.Duration) string {
	return bi(fmt.Sprintf("%s 已解除禁言，计时继续（剩余 %s）。", b(name), FormatDuration(remaining)),
		fmt.Sprintf("%s was unmuted; the timer continues (%s left).", b(name), FormatDurationEN(remaining)))
}

func msgExempt(name string) string {
	return bi(fmt.Sprintf("%s 已由管理员直接授予发言权限，移出麦序队列。", b(name)),
		fmt.Sprintf("%s was allowed to speak by an admin and left the queue.", b(name)))
}

func msgSpeakerOffline(name string, wait, remaining time.Duration) string {
	return bi(fmt.Sprintf("%s 已离线，麦序暂停，等待其重新连接（最多 %s，剩余发言时间 %s）。", b(name), FormatDuration(wait), FormatDuration(remaining)),
		fmt.Sprintf("%s went offline; the queue waits for them to reconnect (up to %s, %s left).", b(name), FormatDurationEN(wait), FormatDurationEN(remaining)))
}

func msgRejoined(name string, bonus, remaining time.Duration) string {
	if bonus > 0 {
		return bi(fmt.Sprintf("%s 已重新连接，计时继续，补偿 %s（剩余 %s）。", b(name), FormatDuration(bonus), FormatDuration(remaining)),
			fmt.Sprintf("%s reconnected; the timer continues with %s extra (%s left).", b(name), FormatDurationEN(bonus), FormatDurationEN(remaining)))
	}
	return bi(fmt.Sprintf("%s 已重新连接，计时继续（剩余 %s）。", b(name), FormatDuration(remaining)),
		fmt.Sprintf("%s reconnected; the timer continues (%s left).", b(name), FormatDurationEN(remaining)))
}

func msgSpeakerDropped(name string, wait, window time.Duration) string {
	zh := fmt.Sprintf("%s 离线超过 %s，轮到下一位。", b(name), FormatDuration(wait))
	en := fmt.Sprintf("%s was offline for over %s; moving on.", b(name), FormatDurationEN(wait))
	if window > 0 {
		zh += fmt.Sprintf("如果 %s 内重新连接，将排在当前发言人之后继续发言。", FormatDuration(window))
		en += fmt.Sprintf(" If they reconnect within %s, they speak right after the current speaker.", FormatDurationEN(window))
	}
	return bi(zh, en)
}

func msgCutIn(name string) string {
	return bi(fmt.Sprintf("%s 已重新连接，排在当前发言人之后继续发言。", b(name)),
		fmt.Sprintf("%s reconnected and speaks right after the current speaker.", b(name)))
}

// Channel warnings: configuration problems an admin has to fix.

func msgACLOpen(names []string) string {
	esc := make([]string, len(names))
	for i, n := range names {
		esc[i] = b(n)
	}
	return warn(bi(fmt.Sprintf("注意：%s 当前没有被禁言。请确认本频道的 ACL 已拒绝 @all 的 Speak（发言）权限，否则麦序无法生效。", strings.Join(esc, "、")),
		fmt.Sprintf("Note: %s can speak freely. Make sure this channel's ACL denies Speak to @all, or the queue has no effect.", strings.Join(esc, ", "))))
}

func msgNoMuteDeafen(name string) string {
	return warn(bi(fmt.Sprintf("无法解除 %s 的禁言，请确认机器人在本频道拥有 MuteDeafen（禁言/耳聋）权限。", b(name)),
		fmt.Sprintf("Cannot let %s speak. Make sure the bot has the MuteDeafen permission in this channel.", b(name))))
}

func msgMoveFailed(name, target string) string {
	return warn(bi(fmt.Sprintf("无法将 %s 转至%s（频道不存在、已满，或机器人缺少 Move 权限），已改为禁言。", b(name), quoteChan(target)),
		fmt.Sprintf("Could not move %s to %s (channel missing or full, or the bot lacks Move); muted instead.", b(name), quoteChanEN(target))))
}

func msgBadTarget(targetID uint32, self bool) string {
	if self {
		return warn(bi(fmt.Sprintf("下麦转频道 %d 就是本频道，发言结束后只能禁言。请修改频道名中的频道 ID。", targetID),
			fmt.Sprintf("Target channel %d is this channel, so finished speakers can only be muted. Please fix the channel ID in the channel name.", targetID)))
	}
	return warn(bi(fmt.Sprintf("下麦转频道 %d 不存在，发言结束后只能禁言。请修改频道名中的频道 ID。", targetID),
		fmt.Sprintf("Target channel %d does not exist, so finished speakers can only be muted. Please fix the channel ID in the channel name.", targetID)))
}

func msgBadCC(ids []uint32) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatUint(uint64(id), 10)
	}
	return warn(bi(fmt.Sprintf("CC 频道 %s 不存在，不会向其发送消息。", strings.Join(s, "、")),
		fmt.Sprintf("CC channel %s does not exist and gets no messages.", strings.Join(s, ", "))))
}

func msgInvalidMarker(err error) string {
	return warn(bi(html.EscapeString(err.Error()),
		"The mic queue marker in the channel name is malformed; the previous settings stay in effect."))
}

// Private messages.

func msgJoinedPrivate(position int, resumed bool) string {
	if resumed {
		return bi(fmt.Sprintf("你已重新连接，排在第 <b>%d</b> 位，轮到你时继续发言（上次剩余时间加补偿）。", position),
			fmt.Sprintf("You reconnected and are number <b>%d</b> in the queue; you will get your remaining time plus compensation.", position))
	}
	return bi(fmt.Sprintf("你已加入麦序队列，当前排在第 <b>%d</b> 位。", position),
		fmt.Sprintf("You joined the queue as number <b>%d</b>.", position))
}

func msgYourTurnPrivate(d time.Duration, target string, resumed bool) string {
	zh := "轮到你发言了，时长 " + countdown(d, FormatDuration) + "。"
	en := "It's your turn to speak: " + countdown(d, FormatDurationEN) + "."
	if resumed {
		zh = "轮到你继续发言了（上次剩余时间加补偿），时长 " + countdown(d, FormatDuration) + "。"
		en = "It's your turn to continue (time left plus compensation): " + countdown(d, FormatDurationEN) + "."
	}
	if target != "" {
		zh += "时间到后将转至" + quoteChan(target) + "。"
		en += " When time is up you will be moved to " + quoteChanEN(target) + "."
	}
	return bi(zh, en)
}

func msgRemainingPrivate(remaining time.Duration) string {
	return bi("你的发言时间还剩 "+countdown(remaining, FormatDuration)+"。",
		"You have "+countdown(remaining, FormatDurationEN)+" left.")
}

func msgNextPrivate(channel string, remaining time.Duration) string {
	return bi(fmt.Sprintf("你是%s的下一位发言者，大约 %s 后轮到你，请做好准备。", quoteChan(channel), countdown(remaining, FormatDuration)),
		fmt.Sprintf("You are next in %s; your turn starts in about %s. Please get ready.", quoteChanEN(channel), countdown(remaining, FormatDurationEN)))
}

func msgTimeUpPrivate(target string) string {
	return bi("你的发言时间已到，已转至"+quoteChan(target)+"。",
		"Your time is up; you have been moved to "+quoteChanEN(target)+".")
}

func msgMutedPrivate(target string) string {
	return bi("你的发言时间已到，但无法将你转至"+quoteChan(target)+"，已改为禁言。离开本频道后会自动解除。",
		"Your time is up, but you could not be moved to "+quoteChanEN(target)+", so you have been muted. Leaving this channel lifts the mute.")
}
