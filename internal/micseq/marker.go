package micseq

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Marker is the configuration embedded in a managed channel's name, e.g.
// "会议室 [麦序/发言时间: 300秒/下麦转12]".
type Marker struct {
	Duration time.Duration
	TargetID uint32
}

var (
	markerLoose  = regexp.MustCompile(`\[\s*麦序\s*/[^\]]*\]`)
	markerStrict = regexp.MustCompile(`^\[\s*麦序\s*/\s*发言时间\s*[:：]\s*([^/\]]+?)\s*/\s*下麦转\s*(\d+)\s*\]$`)
)

// ParseMarker looks for a 麦序 marker in a channel name. found reports whether
// something that looks like a marker is present; err is non-nil when it is
// present but malformed.
func ParseMarker(name string) (m Marker, found bool, err error) {
	raw := markerLoose.FindString(name)
	if raw == "" {
		return Marker{}, false, nil
	}
	sub := markerStrict.FindStringSubmatch(raw)
	if sub == nil {
		return Marker{}, true, fmt.Errorf("麦序标记格式错误：%s（应为 [麦序/发言时间: 300秒/下麦转<频道ID>]）", raw)
	}
	d, err := ParseDuration(sub[1])
	if err != nil {
		return Marker{}, true, err
	}
	target, err := strconv.ParseUint(sub[2], 10, 32)
	if err != nil {
		return Marker{}, true, fmt.Errorf("下麦频道 ID %q 无效", sub[2])
	}
	return Marker{Duration: d, TargetID: uint32(target)}, true, nil
}

// StripMarker returns the channel name without its 麦序 marker.
func StripMarker(name string) string {
	return strings.TrimSpace(markerLoose.ReplaceAllString(name, ""))
}
