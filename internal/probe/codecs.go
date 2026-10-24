package probe

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// CodecString returns a stream's RFC 6381 codecs value in MP4 (or WebM), or ""
// if unknown.
func CodecString(s *Stream, webm bool) string {
	if s == nil {
		return ""
	}
	switch s.CodecName {
	case "h264":
		return avcString(s)
	case "hevc":
		return hevcString(s)
	case "av1":
		return av1String(s)
	case "vp9":
		if webm {
			return "vp9"
		}
		return vp9String(s)
	case "vp8":
		return "vp8"
	case "aac":
		switch s.Profile {
		case "HE-AAC":
			return "mp4a.40.5"
		case "HE-AACv2":
			return "mp4a.40.29"
		}
		return "mp4a.40.2"
	case "mp3":
		return "mp4a.40.34"
	case "opus":
		return "opus"
	case "vorbis":
		return "vorbis"
	case "flac":
		if webm {
			return "flac"
		}
		return "fLaC"
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	}
	return ""
}

// TypeAttr returns a <source type> value such as
// `video/mp4; codecs="avc1.640028, mp4a.40.2"`.
func TypeAttr(mime, codecs string) string {
	if codecs == "" {
		return mime
	}
	return mime + `; codecs="` + codecs + `"`
}

// JoinCodecs joins the non-empty codecs strings of the given streams.
func JoinCodecs(webm bool, streams ...*Stream) string {
	var parts []string
	for _, s := range streams {
		if c := CodecString(s, webm); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, ", ")
}

var avcProfiles = map[string][2]byte{
	"Baseline":              {0x42, 0x00},
	"Constrained Baseline":  {0x42, 0xE0},
	"Main":                  {0x4D, 0x40},
	"Extended":              {0x58, 0x00},
	"High":                  {0x64, 0x00},
	"High 10":               {0x6E, 0x00},
	"High 4:2:2":            {0x7A, 0x00},
	"High 4:4:4 Predictive": {0xF4, 0x00},
}

// avcString uses the avcC record when present: its bytes 1 to 3 are
// profile_idc, the constraint flags and level_idc.
func avcString(s *Stream) string {
	if x := s.Extradata(); len(x) >= 4 && x[0] == 1 {
		return fmt.Sprintf("avc1.%02X%02X%02X", x[1], x[2], x[3])
	}
	p, ok := avcProfiles[s.Profile]
	if !ok {
		p = avcProfiles["High"]
	}
	level := s.Level
	if level <= 0 {
		level = 40
	}
	return fmt.Sprintf("avc1.%02X%02X%02X", p[0], p[1], level)
}

// hevcString follows ISO/IEC 14496-15 annex E, using the hvcC record when
// present.
func hevcString(s *Stream) string {
	tag := "hvc1"
	if s.CodecTagString == "hev1" {
		tag = "hev1"
	}
	x := s.Extradata()
	if len(x) < 13 || x[0] != 1 {
		// No hvcC record: use typical values for the reported profile.
		profile, compat := 1, "6"
		if strings.Contains(s.Profile, "10") {
			profile, compat = 2, "4"
		}
		level := s.Level
		if level <= 0 {
			level = 120
		}
		return fmt.Sprintf("%s.%d.%s.L%d.90", tag, profile, compat, level)
	}
	var b strings.Builder
	b.WriteString(tag)
	b.WriteByte('.')
	if space := x[1] >> 6; space > 0 {
		b.WriteByte("ABC"[space-1])
	}
	b.WriteString(strconv.Itoa(int(x[1] & 0x1F)))
	compat := uint32(x[2])<<24 | uint32(x[3])<<16 | uint32(x[4])<<8 | uint32(x[5])
	fmt.Fprintf(&b, ".%X", bits.Reverse32(compat))
	tier := byte('L')
	if x[1]&0x20 != 0 {
		tier = 'H'
	}
	fmt.Fprintf(&b, ".%c%d", tier, x[12])
	constraints := x[6:12]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	for _, c := range constraints[:last] {
		fmt.Fprintf(&b, ".%X", c)
	}
	return b.String()
}

// av1String uses the av1C record when present.
func av1String(s *Stream) string {
	if x := s.Extradata(); len(x) >= 3 && x[0]&0x80 != 0 {
		profile := x[1] >> 5
		level := x[1] & 0x1F
		tier := byte('M')
		if x[2]&0x80 != 0 {
			tier = 'H'
		}
		depth := 8
		if x[2]&0x40 != 0 {
			depth = 10
			if x[2]&0x20 != 0 {
				depth = 12
			}
		}
		return fmt.Sprintf("av01.%d.%02d%c.%02d", profile, level, tier, depth)
	}
	profile := 0
	switch s.Profile {
	case "High":
		profile = 1
	case "Professional":
		profile = 2
	}
	level := s.Level
	if level < 0 || level > 31 {
		level = 8
	}
	return fmt.Sprintf("av01.%d.%02dM.%02d", profile, level, s.BitDepth())
}

// vp9String builds vp09.PP.LL.DD. MP4 files carry a vpcC box, but ffprobe does
// not expose it, so the level is estimated from the picture size.
func vp9String(s *Stream) string {
	profile := 0
	if _, after, ok := strings.Cut(s.Profile, "Profile "); ok {
		profile, _ = strconv.Atoi(after)
	}
	return fmt.Sprintf("vp09.%02d.%02d.%02d", profile, vp9Level(s.Width*s.Height, s.FPS()), s.BitDepth())
}

func vp9Level(pixels int, fps float64) int {
	high := fps > 30
	switch {
	case pixels <= 36864:
		return 10
	case pixels <= 73728:
		return 11
	case pixels <= 122880:
		return 20
	case pixels <= 245760:
		return 21
	case pixels <= 552960:
		return 30
	case pixels <= 983040:
		return 31
	case pixels <= 2228224:
		if high {
			return 41
		}
		return 40
	case pixels <= 8912896:
		if high {
			return 51
		}
		return 50
	}
	return 60
}
