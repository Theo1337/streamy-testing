package encode

import (
	"fmt"
	"strconv"
	"strings"

	"streamy/internal/capture"
	"streamy/internal/config"
)

// Output is where the encoded video goes.
type Output struct {
	// Path is the file to write. Unused when Pipe is set.
	Path string

	// Format names the container or raw stream format: "mp4", "mkv" or "h264".
	// Empty means "mp4" for a file and "h264" for a pipe.
	Format string

	// Pipe sends the encoded stream to ffmpeg's stdout so Go can read it. This
	// is the path the WebRTC sender takes in phase 3, and what the debug tool's
	// -stdout flag exercises.
	//
	// Raw H.264 has no container at all: it is the byte stream a WebRTC
	// packetiser expects, and it cannot be scrubbed or played by double
	// clicking, which is why it is not the default for a recording.
	Pipe bool
}

// FormatOrDefault returns the format to use.
func (o Output) FormatOrDefault() string {
	if o.Format != "" {
		return o.Format
	}
	if o.Pipe {
		return "h264"
	}
	return "mp4"
}

// Args assembles the whole ffmpeg command line for one capture.
//
// The order is not arbitrary: global flags, then the input, then video filters,
// then encoder settings, then the output. Encoder settings have to come after
// the input because some of them are resolved against the input's pixel format.
func Args(cfg config.Config, target capture.Target, enc Encoder, caps capture.Capabilities, out Output) ([]string, error) {
	return ArgsWithInput(cfg, target, enc, caps, out, nil)
}

// InputOverride replaces the target-derived ffmpeg input. Window capture uses
// it because its frames come from Go over a loopback socket rather than from
// ddagrab or gdigrab.
type InputOverride struct {
	Args    []string
	Filters string
}

// ArgsWithInput is Args with an optional pre-built input.
func ArgsWithInput(cfg config.Config, target capture.Target, enc Encoder, caps capture.Capabilities, out Output, override *InputOverride) ([]string, error) {
	fps := cfg.Encoding.FPS
	if fps <= 0 {
		fps = 30
	}

	var input InputOverride
	if override != nil {
		input = *override
	} else {
		grabber := target.ChooseGrabber(caps)
		spec, err := target.InputSpec(capture.InputOptions{
			Grabber:   grabber,
			FPS:       fps,
			DrawMouse: true, // cursor is always shown since the setting was removed
			Scale:     resolutionScale(cfg.Encoding.Resolution),
		})
		if err != nil {
			return nil, fmt.Errorf("build capture input for %s: %w", target.Describe(), err)
		}
		input = InputOverride{Args: spec.Args, Filters: spec.Filters}
	}

	args := []string{
		"-hide_banner",
		// The progress line is written with carriage returns and redrawn
		// constantly; the runner is reading stderr into a buffer to explain
		// failures, so it is pure noise.
		"-nostats",
		"-loglevel", ffmpegLogLevel(cfg.LogLevel),
	}
	args = append(args, input.Args...)
	// Keep the screen-capture process video-only. Desktop audio uses a separate
	// WASAPI loopback process and is sent as RTP to the shared WebRTC audio track.
	args = append(args, "-an")
	if input.Filters != "" {
		args = append(args, "-vf", input.Filters)
	}
	args = append(args, videoArgs(cfg, enc, fps)...)
	args = append(args, outputArgs(out)...)
	return args, nil
}

// videoArgs builds the encoder half of the command line.
func videoArgs(cfg config.Config, enc Encoder, fps int) []string {
	bitrate := cfg.Encoding.BitrateKbps
	if bitrate <= 0 {
		bitrate = 6000
	}
	bufsize := bitrate * 2

	gop := fps * cfg.Encoding.KeyframeSeconds
	if cfg.Encoding.KeyframeSeconds <= 0 || gop <= 0 {
		gop = fps
	}

	args := []string{
		"-c:v", enc.Name,
		// Every encoder gets the same rate control shape: a target and a hard
		// ceiling. The ceiling is what stops a burst of motion from producing
		// more data than a viewer's connection can carry.
		"-b:v", strconv.Itoa(bitrate) + "k",
		"-maxrate", strconv.Itoa(bitrate) + "k",
		"-bufsize", strconv.Itoa(bufsize) + "k",
		// A keyframe roughly once a second is what lets a viewer who joins in
		// the middle wait no more than a second before the picture appears.
		"-g", strconv.Itoa(gop),
		// No B frames: they cost output frames of latency, and a live viewer
		// gains nothing from them.
		"-bf", "0",
		// 8 bit 4:2:0 is the lowest common denominator that every browser and
		// every hardware decoder handles.
		"-pix_fmt", "yuv420p",
	}

	switch enc.Label {
	case config.EncoderNVENC:
		// p1 is the fastest preset, "ll" tunes for latency, cbr keeps the bit
		// rate flat, and the lookahead is off because it delays output.
		args = append(args,
			"-preset", "p1",
			"-tune", "ll",
			"-rc", "cbr",
			"-rc-lookahead", "0",
			"-zerolatency", "1",
			"-delay", "0",
			"-forced-idr", "1",
			"-profile:v", "baseline",
		)

	case config.EncoderQSV:
		args = append(args,
			"-preset", "veryfast",
			"-look_ahead", "0",
			"-profile:v", "baseline",
		)

	case config.EncoderAMF:
		// AMF names its profiles differently: `-profile:v baseline` is rejected
		// outright as an unknown value, and constrained_baseline is what AMF
		// calls the profile a WebRTC viewer wants anyway.
		args = append(args,
			"-usage", "lowlatency",
			"-quality", "speed",
			"-rc", "cbr",
			"-profile:v", "constrained_baseline",
		)

	default: // libx264
		args = append(args,
			"-preset", "ultrafast",
			// zerolatency disables lookahead, B frames and frame threading,
			// which is exactly the latency a live stream cannot afford.
			"-tune", "zerolatency",
			"-profile:v", "baseline",
		)
		if threads := cfg.Encoding.EncoderThreads; threads > 0 {
			args = append(args, "-threads", strconv.Itoa(threads))
		}
	}

	args = append(args, cfg.Encoding.ExtraEncoderArgs...)
	return args
}

// ValidateInput builds the ffmpeg input for a target without starting ffmpeg.
//
// A switch prepares the next input before stopping the current capture, so a
// target that cannot be captured (for example a monitor with no DXGI output)
// fails the switch instead of taking a healthy stream down with it.
func ValidateInput(cfg config.Config, target capture.Target, enc Encoder, caps capture.Capabilities, input *InputOverride) error {
	_, err := ArgsWithInput(cfg, target, enc, caps, Output{Pipe: true, Format: "h264"}, input)
	return err
}

// outputArgs builds the output half of the command line.
func outputArgs(out Output) []string {
	switch format := out.FormatOrDefault(); format {
	case "h264":
		// An elementary H.264 stream, with no container. This is what a WebRTC
		// packetiser consumes, and what `ffplay -` can play straight from a
		// pipe.
		return []string{"-f", "h264", "-"}

	case "mp4":
		// A plain MP4. It only becomes playable once ffmpeg writes the index at
		// the end, which is why the runner stops ffmpeg with "q" on stdin
		// instead of killing it: a killed ffmpeg leaves an unplayable file.
		return []string{"-f", "mp4", out.Path}

	case "mkv":
		// Matroska is worth offering because it survives a hard kill: there is
		// no trailing index to lose.
		return []string{"-f", "matroska", out.Path}

	case "mp4-frag":
		// Fragmented MP4 also survives a hard kill, at the cost of worse
		// seeking in some players.
		return []string{"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov", out.Path}

	default:
		return []string{"-f", format, out.Path}
	}
}

// resolutionScale turns encoding.resolution into a scale filter argument.
//
// "source" means "leave it alone". Anything else is a WIDTHxHEIGHT string, and
// the config validator has already checked its shape.
func resolutionScale(res string) string {
	res = strings.ToLower(strings.TrimSpace(res))
	if res == "" || res == "source" {
		return ""
	}
	return res
}

// ffmpegLogLevel maps our log level onto ffmpeg's.
//
// Ours is coarser: a stream running at "info" should not flood the log with
// ffmpeg's chatty per-stream notes, but "debug" is exactly when those notes are
// the point.
func ffmpegLogLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return "info"
	case "error":
		return "error"
	default:
		return "warning"
	}
}
