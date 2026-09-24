package micseq

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Marker is the configuration embedded in a managed channel's name, e.g.
// "会议室 [麦序/发言时间: 300秒/下麦转12/CC 13,14]".
type Marker struct {
	Duration time.Duration
	TargetID uint32
	// CC lists channels that receive a copy of every channel-wide message.
	CC []uint32
}

// Equal reports whether two markers describe the same settings.
func (m Marker) Equal(o Marker) bool {
	return m.Duration == o.Duration && m.TargetID == o.TargetID && slices.Equal(m.CC, o.CC)
}

var (
	markerLoose = regexp.MustCompile(`\[\s*麦序\s*/[^\]]*\]`)
	markerBody  = regexp.MustCompile(`^\[\s*麦序\s*/(.*)\]$`)
	segDuration = regexp.MustCompile(`^发言时间\s*[:：]\s*(.+)$`)
	segTarget   = regexp.MustCompile(`^下麦转\s*[:：]?\s*(\d+)$`)
	segCC       = regexp.MustCompile(`(?i)^CC\s*[:：]?\s*(.*)$`)
	ccSeparator = regexp.MustCompile(`[,，、\s]+`)
)

const markerUsage = "应为 [麦序/发言时间: 300秒/下麦转<频道ID>]，可选追加 /CC <频道ID>,<频道ID>"

// ParseMarker looks for a 麦序 marker in a channel name. found reports whether
// something that looks like a marker is present; err is non-nil when it is
// present but malformed.
func ParseMarker(name string) (m Marker, found bool, err error) {
	raw := markerLoose.FindString(name)
	if raw == "" {
		return Marker{}, false, nil
	}
	formatErr := fmt.Errorf("麦序标记格式错误：%s（%s）", raw, markerUsage)
	body := markerBody.FindStringSubmatch(raw)
	if body == nil {
		return Marker{}, true, formatErr
	}
	var haveDuration, haveTarget, haveCC bool
	for _, seg := range strings.Split(body[1], "/") {
		seg = strings.TrimSpace(seg)
		switch {
		case segDuration.MatchString(seg):
			if haveDuration {
				return Marker{}, true, formatErr
			}
			haveDuration = true
			m.Duration, err = ParseDuration(segDuration.FindStringSubmatch(seg)[1])
			if err != nil {
				return Marker{}, true, err
			}
		case segTarget.MatchString(seg):
			if haveTarget {
				return Marker{}, true, formatErr
			}
			haveTarget = true
			m.TargetID, err = parseChannelID(segTarget.FindStringSubmatch(seg)[1], "下麦频道")
			if err != nil {
				return Marker{}, true, err
			}
		case segCC.MatchString(seg):
			if haveCC {
				return Marker{}, true, formatErr
			}
			haveCC = true
			m.CC, err = parseCC(segCC.FindStringSubmatch(seg)[1])
			if err != nil {
				return Marker{}, true, err
			}
		default:
			return Marker{}, true, formatErr
		}
	}
	if !haveDuration || !haveTarget {
		return Marker{}, true, formatErr
	}
	return m, true, nil
}

func parseChannelID(s, what string) (uint32, error) {
	id, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s ID %q 无效", what, s)
	}
	return uint32(id), nil
}

func parseCC(list string) ([]uint32, error) {
	var ids []uint32
	for _, part := range ccSeparator.Split(strings.TrimSpace(list), -1) {
		if part == "" {
			continue
		}
		id, err := parseChannelID(part, "CC 频道")
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("CC 后面需要至少一个频道 ID，例如 /CC 13,14")
	}
	return ids, nil
}

// StripMarker returns the channel name without its 麦序 marker.
func StripMarker(name string) string {
	return strings.TrimSpace(markerLoose.ReplaceAllString(name, ""))
}
