package micseq

import (
	"fmt"
	"html"
	"strings"
	"time"
)

const msgPrefix = "【麦序】"

// maxListed caps how many queue entries are printed in one message.
const maxListed = 30

func b(name string) string {
	return "<b>" + html.EscapeString(name) + "</b>"
}

func quoteChan(name string) string {
	return "「" + html.EscapeString(name) + "」"
}

func say(format string, args ...any) string {
	return msgPrefix + fmt.Sprintf(format, args...)
}

func msgTakeover(d time.Duration, target string) string {
	return say("本频道已启用麦序模式：按进入顺序依次发言，每人 %s，结束后转至%s。", FormatDuration(d), quoteChan(target))
}

func msgResumeAfterRestart() string {
	return say("机器人已重新连接，麦序继续。")
}

func msgStopped() string {
	return say("本频道的麦序模式已关闭。")
}

func msgTurnStart(name string, d time.Duration, resumed bool, next *member) string {
	s := say("轮到 %s 发言，时长 %s。", b(name), FormatDuration(d))
	if resumed {
		s = say("轮到 %s 继续发言（上次剩余时间加补偿），时长 %s。", b(name), FormatDuration(d))
	}
	if next != nil {
		s += "下一位：" + b(next.Name) + "。"
	}
	return s
}

func msgQueueEmpty() string {
	return say("当前无人排队，等待新成员进入频道。")
}

func msgTimeUp(name, target string) string {
	return say("%s 的发言时间已到，已转至%s。", b(name), quoteChan(target))
}

func msgLeftEarly(name string) string {
	return say("%s 已离开频道，提前结束发言。", b(name))
}

func msgPaused(name string, remaining time.Duration) string {
	return say("%s 已被管理员禁言，计时暂停（剩余 %s）。", b(name), FormatDuration(remaining))
}

func msgResumed(name string, remaining time.Duration) string {
	return say("%s 已解除禁言，计时继续（剩余 %s）。", b(name), FormatDuration(remaining))
}

func msgExempt(name string) string {
	return say("%s 已由管理员直接授予发言权限，移出麦序队列。", b(name))
}

func msgWarnNext(speaker, next string, remaining time.Duration) string {
	return say("%s 的发言还剩 %s，下一位 %s 请做好准备。", b(speaker), FormatDuration(remaining), b(next))
}

func msgWarnNextPrivate(channel string, remaining time.Duration) string {
	return say("你是%s的下一位发言者，大约 %s 后轮到你，请做好准备。", quoteChan(channel), FormatDuration(remaining))
}

func msgRemaining(name string, remaining time.Duration) string {
	return say("%s 剩余发言时间 %s。", b(name), FormatDuration(remaining))
}

func msgSpeakerOffline(name string, wait, remaining time.Duration) string {
	return say("%s 已离线，麦序暂停，等待其重新连接（最多 %s，剩余发言时间 %s）。", b(name), FormatDuration(wait), FormatDuration(remaining))
}

func msgRejoined(name string, bonus, remaining time.Duration) string {
	if bonus > 0 {
		return say("%s 已重新连接，计时继续，补偿 %s（剩余 %s）。", b(name), FormatDuration(bonus), FormatDuration(remaining))
	}
	return say("%s 已重新连接，计时继续（剩余 %s）。", b(name), FormatDuration(remaining))
}

func msgSpeakerDropped(name string, wait, window time.Duration) string {
	s := say("%s 离线超过 %s，轮到下一位。", b(name), FormatDuration(wait))
	if window > 0 {
		s += fmt.Sprintf("如果 %s 内重新连接，将排在当前发言人之后继续发言。", FormatDuration(window))
	}
	return s
}

func msgCutIn(name string) string {
	return say("%s 已重新连接，排在当前发言人之后继续发言。", b(name))
}

type statusLine struct {
	Name    string
	Offline bool
}

func msgStatus(speaker *member, remaining time.Duration, note string, queue []statusLine) string {
	var sb strings.Builder
	sb.WriteString(msgPrefix)
	if speaker != nil {
		sb.WriteString("当前发言：" + b(speaker.Name) + "（剩余 " + FormatDuration(remaining))
		if note != "" {
			sb.WriteString("，" + note)
		}
		sb.WriteString("）")
	} else {
		sb.WriteString("当前无人发言")
	}
	if len(queue) == 0 {
		sb.WriteString("<br>排队：无")
		return sb.String()
	}
	fmt.Fprintf(&sb, "<br>排队（%d 人）：", len(queue))
	for i, q := range queue {
		if i == maxListed {
			fmt.Fprintf(&sb, "<br>……另有 %d 人", len(queue)-maxListed)
			break
		}
		fmt.Fprintf(&sb, "<br>%d. %s", i+1, html.EscapeString(q.Name))
		if q.Offline {
			sb.WriteString("（断线，等待重连）")
		}
	}
	return sb.String()
}

func msgACLOpen(names []string) string {
	esc := make([]string, len(names))
	for i, n := range names {
		esc[i] = b(n)
	}
	return say("注意：%s 当前没有被禁言。请确认本频道的 ACL 已拒绝 @all 的 Speak（发言）权限，否则麦序无法生效。", strings.Join(esc, "、"))
}

func msgNoMuteDeafen(name string) string {
	return say("无法解除 %s 的禁言，请确认机器人在本频道拥有 MuteDeafen（禁言/耳聋）权限。", b(name))
}

func msgMoveFailed(name, target string) string {
	return say("无法将 %s 转至%s（频道不存在、已满，或机器人缺少 Move 权限），已改为禁言。", b(name), quoteChan(target))
}

func msgBadTarget(targetID uint32, self bool) string {
	if self {
		return say("下麦转频道 %d 就是本频道，发言结束后只能禁言。请修改频道名中的频道 ID。", targetID)
	}
	return say("下麦转频道 %d 不存在，发言结束后只能禁言。请修改频道名中的频道 ID。", targetID)
}

func msgInvalidMarker(err error) string {
	return say("%s", html.EscapeString(err.Error()))
}
