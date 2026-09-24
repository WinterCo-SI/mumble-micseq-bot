package micseq

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxDuration is the longest speaking time accepted in a channel marker.
const MaxDuration = 24 * time.Hour

var durationPart = regexp.MustCompile(`^(\d+)\s*(小时|分钟|hours|hour|mins|min|secs|sec|hrs|hr|时|分|秒|h|m|s)?`)

var unitScale = map[string]time.Duration{
	"":      time.Second,
	"s":     time.Second,
	"sec":   time.Second,
	"secs":  time.Second,
	"秒":     time.Second,
	"m":     time.Minute,
	"min":   time.Minute,
	"mins":  time.Minute,
	"分":     time.Minute,
	"分钟":    time.Minute,
	"h":     time.Hour,
	"hr":    time.Hour,
	"hrs":   time.Hour,
	"hour":  time.Hour,
	"hours": time.Hour,
	"时":     time.Hour,
	"小时":    time.Hour,
}

// ParseDuration parses speaking times such as "300", "50s", "50秒", "10分钟",
// "5min", "1h" and "1分30秒". A number without a unit means seconds, including
// a trailing unitless part ("1分30" is 90 seconds).
func ParseDuration(s string) (time.Duration, error) {
	rest := strings.TrimSpace(strings.ToLower(s))
	if rest == "" {
		return 0, errors.New("发言时间为空")
	}
	var total time.Duration
	for rest != "" {
		m := durationPart.FindStringSubmatch(rest)
		if m == nil {
			return 0, fmt.Errorf("无法识别的发言时间 %q", s)
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n > int64(MaxDuration/time.Second) {
			return 0, fmt.Errorf("发言时间 %q 过长", s)
		}
		total += time.Duration(n) * unitScale[m[2]]
		if total > MaxDuration {
			return 0, fmt.Errorf("发言时间 %q 超过 24 小时", s)
		}
		rest = strings.TrimSpace(rest[len(m[0]):])
	}
	if total <= 0 {
		return 0, fmt.Errorf("发言时间 %q 必须大于 0", s)
	}
	return total, nil
}

// FormatDuration renders a duration in Chinese, e.g. "4分30秒".
// Durations are rounded up to whole seconds.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64((d + time.Second - 1) / time.Second)
	h, m, s := secs/3600, secs/60%60, secs%60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%d小时", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%d分", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%d秒", s)
	}
	if h == 0 && m > 0 && s == 0 {
		b.WriteString("钟")
	}
	return b.String()
}
