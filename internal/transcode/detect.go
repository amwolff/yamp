package transcode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a command and returns its standard output. Tests replace it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs a command; its error includes the last line of stderr.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: ffmpeg with arguments that YAMP builds; no shell
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return out, fmt.Errorf("%w: %s", err, msg)
	}
	return out, nil
}

// EncoderStatus is the detection result of one encoder.
type EncoderStatus struct {
	Name      string
	Codec     string
	Hardware  bool
	Available bool
	Detail    string
}

// Caps describes what the local ffmpeg can do.
type Caps struct {
	Encoders    []EncoderStatus
	Filters     map[string]bool
	VAAPIDevice string
}

// candidates is the display order of the encoders.
var candidates = []string{"libx264", "h264_qsv", "h264_vaapi", "libx265", "hevc_qsv", "hevc_vaapi", "libsvtav1"}

func isVAAPI(enc string) bool { return strings.HasSuffix(enc, "_vaapi") }
func isQSV(enc string) bool   { return strings.HasSuffix(enc, "_qsv") }

// Detect lists ffmpeg's encoders and filters. -encoders also lists hardware
// encoders without a usable device, so each candidate encodes three frames.
func Detect(ctx context.Context, run Runner, ffmpeg, vaapiDevice string) *Caps {
	caps := &Caps{Filters: map[string]bool{}, VAAPIDevice: vaapiDevice}
	compiled := map[string]bool{}
	if out, err := run(ctx, ffmpeg, "-hide_banner", "-encoders"); err == nil {
		compiled = listNames(out)
	}
	if out, err := run(ctx, ffmpeg, "-hide_banner", "-filters"); err == nil {
		caps.Filters = listNames(out)
	}
	for _, enc := range candidates {
		st := EncoderStatus{Name: enc, Codec: EncoderCodec[enc], Hardware: isVAAPI(enc) || isQSV(enc)}
		switch {
		case !compiled[enc]:
			st.Detail = "not in this ffmpeg build"
		default:
			tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := run(tctx, ffmpeg, testEncodeArgs(enc, vaapiDevice)...)
			cancel()
			if err != nil {
				st.Detail = err.Error()
			} else {
				st.Available = true
				st.Detail = "test encode passed"
			}
		}
		caps.Encoders = append(caps.Encoders, st)
	}
	return caps
}

func testEncodeArgs(enc, vaapiDevice string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if isVAAPI(enc) {
		args = append(args, "-init_hw_device", "vaapi=va:"+vaapiDevice, "-filter_hw_device", "va")
	}
	args = append(args, "-f", "lavfi", "-i", "color=black:s=256x256:r=25:d=0.2", "-frames:v", "3")
	switch {
	case isVAAPI(enc):
		args = append(args, "-vf", "format=nv12,hwupload")
	case isQSV(enc):
		args = append(args, "-vf", "format=nv12")
	default:
		args = append(args, "-pix_fmt", "yuv420p")
	}
	return append(args, "-c:v", enc, "-f", "null", "-")
}

// listNames parses the table that ffmpeg -encoders and -filters print: the
// second column is the name.
func listNames(out []byte) map[string]bool {
	names := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && !strings.HasSuffix(f[0], ":") && f[0] != "=" {
			names[f[1]] = true
		}
	}
	return names
}

// Has reports whether the encoder passed detection.
func (c *Caps) Has(enc string) bool {
	for _, e := range c.Encoders {
		if e.Name == enc {
			return e.Available
		}
	}
	return false
}

// Available returns the names of the encoders that passed detection.
func (c *Caps) Available() []string {
	var names []string
	for _, e := range c.Encoders {
		if e.Available {
			names = append(names, e.Name)
		}
	}
	return names
}

// CanTonemap reports whether the software tone-mapping chain is available.
func (c *Caps) CanTonemap() bool {
	return c.Filters["zscale"] && c.Filters["tonemap"]
}

// Resolve returns the encoder to use for codec. "auto" picks the first
// available encoder, hardware first.
func (c *Caps) Resolve(codec, requested string) (string, error) {
	if requested != "auto" {
		if !c.Has(requested) {
			return "", fmt.Errorf("encoder %s is not available on this host", requested)
		}
		return requested, nil
	}
	for _, enc := range autoOrder[codec] {
		if c.Has(enc) {
			return enc, nil
		}
	}
	return "", fmt.Errorf("no encoder for %s is available on this host", codec)
}
