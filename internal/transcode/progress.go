package transcode

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Progress is one block of ffmpeg -progress output.
type Progress struct {
	OutTime time.Duration
	Speed   float64
	// End is true for the last block (progress=end).
	End bool
}

// ParseProgress calls fn for each block of ffmpeg -progress output: key=value
// lines ending in progress=continue or progress=end. "N/A" values are ignored.
func ParseProgress(r io.Reader, fn func(Progress)) error {
	sc := bufio.NewScanner(r)
	var cur Progress
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_us", "out_time_ms":
			// ffmpeg prints microseconds for both keys.
			if us, err := strconv.ParseInt(val, 10, 64); err == nil && us >= 0 {
				cur.OutTime = time.Duration(us) * time.Microsecond
			}
		case "speed":
			if f, err := strconv.ParseFloat(strings.TrimSuffix(val, "x"), 64); err == nil {
				cur.Speed = f
			}
		case "progress":
			cur.End = val == "end"
			fn(cur)
		}
	}
	return sc.Err()
}
