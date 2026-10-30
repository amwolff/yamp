package web

import (
	"fmt"
	"html/template"
	"math"
	"time"

	"github.com/amwolff/yamp/internal/transcode"
)

var funcs = template.FuncMap{
	"duration":   formatDuration,
	"bytes":      formatBytes,
	"percent":    func(f float64) int { return int(math.Round(f * 100)) },
	"stars":      func(n int64) []bool { return []bool{n >= 1, n >= 2, n >= 3, n >= 4, n >= 5} },
	"unix":       func(t int64) time.Time { return time.Unix(t, 0) },
	"ago":        ago,
	"hours":      formatHours,
	"resolution": resolutionLabel,
	"add":        func(a, b int) int64 { return int64(a + b) },
	"isAdmin": func(nav string) bool {
		return nav != "" && nav != "home" && nav != "watch"
	},
	"classHelp": func(class string) string { return classHelp[class] },
	"encoderOK": func(c *transcode.Caps, name string) bool { return name == "auto" || c.Has(name) },
}

var classHelp = map[string]string{
	"direct":      "Every browser plays the original file.",
	"conditional": "Some browsers play the original (HEVC, AV1, VP9). Others need a transcode.",
	"remux":       "The codecs are compatible, but the container is not. A copy into MP4 is sufficient.",
	"audio":       "Only the audio needs a new encode.",
	"transcode":   "The video needs a new encode.",
}

// formatDuration formats milliseconds as h:mm:ss or m:ss.
func formatDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	sec := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

// formatHours formats a long duration for statistics.
func formatHours(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return fmt.Sprintf("%.1f h", d.Hours())
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t int64) string {
	if t == 0 {
		return "never"
	}
	d := time.Since(time.Unix(t, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

// resolutionLabel names common heights.
func resolutionLabel(w, h int64) string {
	short := min(w, h)
	switch {
	case short >= 2000:
		return "4K"
	case short >= 1400:
		return "1440p"
	case short >= 1000:
		return "1080p"
	case short >= 700:
		return "720p"
	case short >= 560:
		return "576p"
	case short >= 460:
		return "480p"
	case short > 0:
		return fmt.Sprintf("%dp", short)
	}
	return ""
}
