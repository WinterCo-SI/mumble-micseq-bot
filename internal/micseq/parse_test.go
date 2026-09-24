package micseq

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"300", 300 * time.Second},
		{" 300 ", 300 * time.Second},
		{"300秒", 300 * time.Second},
		{"50s", 50 * time.Second},
		{"50 S", 50 * time.Second},
		{"50sec", 50 * time.Second},
		{"50秒", 50 * time.Second},
		{"10分钟", 10 * time.Minute},
		{"10分", 10 * time.Minute},
		{"5min", 5 * time.Minute},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"2小时", 2 * time.Hour},
		{"1分30秒", 90 * time.Second},
		{"1分30", 90 * time.Second},
		{"1m30s", 90 * time.Second},
		{"1 分钟 30 秒", 90 * time.Second},
		{"1小时30分钟", 90 * time.Minute},
		{"24h", 24 * time.Hour},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if err != nil {
			t.Errorf("ParseDuration(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseDurationInvalid(t *testing.T) {
	for _, in := range []string{"", "  ", "0", "0秒", "abc", "5天", "秒", "25h", "5ms", "-5", "1.5分钟", "99999999999999999999"} {
		if d, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want error", in, d)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0秒"},
		{-time.Second, "0秒"},
		{500 * time.Millisecond, "1秒"},
		{45 * time.Second, "45秒"},
		{5 * time.Minute, "5分钟"},
		{270 * time.Second, "4分30秒"},
		{time.Hour, "1小时"},
		{90 * time.Minute, "1小时30分"},
		{time.Hour + 5*time.Second, "1小时5秒"},
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.in); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseMarker(t *testing.T) {
	tests := []struct {
		name  string
		found bool
		ok    bool
		want  Marker
	}{
		{"会议室 [麦序/发言时间: 300秒/下麦转12]", true, true, Marker{300 * time.Second, 12}},
		{"会议室 [麦序/发言时间: 300/下麦转5]", true, true, Marker{300 * time.Second, 5}},
		{"会议室 [麦序/发言时间：10分钟/下麦转7]", true, true, Marker{10 * time.Minute, 7}},
		{"会议室 [ 麦序 / 发言时间 : 50s / 下麦转 3 ]", true, true, Marker{50 * time.Second, 3}},
		{"[麦序/发言时间:1分30/下麦转0] 大厅", true, true, Marker{90 * time.Second, 0}},
		{"会议室", false, true, Marker{}},
		{"会议室 [闲聊]", false, true, Marker{}},
		{"会议室 [麦序/发言时间: 300秒]", true, false, Marker{}},
		{"会议室 [麦序/发言时间: 五分钟/下麦转12]", true, false, Marker{}},
		{"会议室 [麦序/发言时间: 300秒/下麦转abc]", true, false, Marker{}},
		{"会议室 [麦序/发言时间: 0/下麦转12]", true, false, Marker{}},
	}
	for _, tt := range tests {
		m, found, err := ParseMarker(tt.name)
		if found != tt.found || (err == nil) != tt.ok {
			t.Errorf("ParseMarker(%q) found=%v err=%v, want found=%v ok=%v", tt.name, found, err, tt.found, tt.ok)
			continue
		}
		if found && tt.ok && m != tt.want {
			t.Errorf("ParseMarker(%q) = %+v, want %+v", tt.name, m, tt.want)
		}
	}
}

func TestStripMarker(t *testing.T) {
	if got := StripMarker("会议室 [麦序/发言时间: 300秒/下麦转12]"); got != "会议室" {
		t.Errorf("StripMarker = %q", got)
	}
	if got := StripMarker("大厅"); got != "大厅" {
		t.Errorf("StripMarker = %q", got)
	}
}

func TestNameLessPinyin(t *testing.T) {
	less := NameLess(true)
	if !less("李四", "王五") || !less("王五", "张三") || !less("alice", "Bob") {
		t.Error("pinyin order wrong")
	}
	plain := NameLess(false)
	if !plain("alice", "Bob") {
		t.Error("case-insensitive order wrong")
	}
}
