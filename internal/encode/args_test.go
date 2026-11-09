package encode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
)

// monitorTarget is a two monitor desktop's second screen, rotated.
func monitorTarget() capture.Target {
	return capture.Target{
		Kind: capture.KindMonitor,
		Monitor: capture.Monitor{
			Index:     1,
			DXGIIndex: 3,
			Bounds:    capture.Rect{X: 1920, Y: 0, Width: 2560, Height: 1440},
			Rotation:  90,
		},
		Reason: "test",
	}
}

func windowTarget() capture.Target {
	return capture.Target{
		Kind:    capture.KindWindow,
		Window:  capture.Window{Handle: 42, Title: "Notepad", Rect: capture.Rect{X: 100, Y: 50, Width: 800, Height: 600}},
		Monitor: capture.Monitor{Index: 0, DXGIIndex: 0, Bounds: capture.Rect{Width: 1920, Height: 1080}},
	}
}

func TestArgsDDAGrabMonitor(t *testing.T) {
	cfg := config.Default()
	cfg.Encoding.BitrateKbps = 4000
	cfg.Encoding.KeyframeSeconds = 2

	args, err := Args(cfg, monitorTarget(), EncoderNVENC, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	line := strings.Join(args, " ")

	for _, want := range []string{
		"-init_hw_device d3d11va",
		"-f lavfi -i ddagrab=output_idx=3:framerate=30:draw_mouse=1",
		"-an",
		"-vf hwdownload,format=bgra,transpose=1",
		"-c:v h264_nvenc",
		"-b:v 4000k",
		"-maxrate 4000k",
		"-bufsize 8000k",
		"-g 60", // 30 fps times 2 seconds
		"-bf 0",
		"-pix_fmt yuv420p",
		"-f mp4 out.mp4",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("args are missing %q\nfull: %s", want, line)
		}
	}
}

func TestArgsGDigrabMonitorWhenDDAGrabUnavailable(t *testing.T) {
	cfg := config.Default()

	args, err := Args(cfg, monitorTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	line := strings.Join(args, " ")

	if !strings.Contains(line, "-offset_x 1920") || !strings.Contains(line, "-video_size 2560x1440") {
		t.Errorf("the fallback does not select the monitor region: %s", line)
	}
	if strings.Contains(line, "ddagrab") {
		t.Errorf("ddagrab was used even though it is unavailable: %s", line)
	}
	// GDI sees the desktop already rotated, so a rotated monitor must not be
	// transposed here.
	if strings.Contains(line, "transpose") {
		t.Errorf("a GDI capture was transposed: %s", line)
	}
}

func TestArgsWindowTargetNeverUsesTheWindowTitle(t *testing.T) {
	cfg := config.Default()

	args, err := Args(cfg, windowTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	line := strings.Join(args, " ")

	// GDI's title= path returns black for GPU-composited windows, so the
	// window is grabbed as the rectangle it occupies instead.
	if strings.Contains(line, "title=") {
		t.Errorf("the window is still selected by title: %s", line)
	}
	for _, want := range []string{"-offset_x 100", "-offset_y 50", "-video_size 800x600", "-i desktop"} {
		if !strings.Contains(line, want) {
			t.Errorf("args are missing %q\nfull: %s", want, line)
		}
	}
}

func TestArgsWindowTargetCropsDDAGrab(t *testing.T) {
	cfg := config.Default()

	args, err := Args(cfg, windowTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	line := strings.Join(args, " ")

	if !strings.Contains(line, "ddagrab=output_idx=0") {
		t.Errorf("the window's monitor is not duplicated: %s", line)
	}
	if !strings.Contains(line, "crop=800:600:100:50") {
		t.Errorf("the window rectangle is not cropped out of the frame: %s", line)
	}
}

func TestArgsEncoderSpecificFlags(t *testing.T) {
	cfg := config.Default()

	cases := []struct {
		encoder Encoder
		want    []string
		avoid   []string
	}{
		{
			encoder: EncoderNVENC,
			want:    []string{"-preset p1", "-tune ll", "-rc cbr", "-zerolatency 1", "-profile:v baseline"},
		},
		{
			encoder: EncoderQSV,
			want:    []string{"-preset veryfast", "-look_ahead 0", "-profile:v baseline"},
		},
		{
			encoder: EncoderAMF,
			// AMF rejects `-profile:v baseline`; constrained_baseline is its
			// name for the same thing.
			want:  []string{"-usage lowlatency", "-rc cbr", "-profile:v constrained_baseline"},
			avoid: []string{"-profile:v baseline"},
		},
		{
			encoder: EncoderX264,
			want:    []string{"-preset ultrafast", "-tune zerolatency", "-profile:v baseline"},
		},
	}

	for _, c := range cases {
		t.Run(c.encoder.Label, func(t *testing.T) {
			args, err := Args(cfg, monitorTarget(), c.encoder, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Pipe: true})
			if err != nil {
				t.Fatalf("Args: %v", err)
			}
			line := strings.Join(args, " ")

			if !strings.Contains(line, "-c:v "+c.encoder.Name) {
				t.Errorf("the encoder is not selected: %s", line)
			}
			for _, want := range c.want {
				if !strings.Contains(line, want) {
					t.Errorf("missing %q\nfull: %s", want, line)
				}
			}
			for _, avoid := range c.avoid {
				if strings.Contains(line, avoid) {
					t.Errorf("contains %q, which this encoder rejects\nfull: %s", avoid, line)
				}
			}
		})
	}
}

func TestArgsExtraEncoderArgsComeLast(t *testing.T) {
	cfg := config.Default()
	cfg.Encoding.ExtraEncoderArgs = []string{"-cq", "20"}

	args, err := Args(cfg, monitorTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}

	// User supplied arguments have to win over ours, which they only do if they
	// come after them.
	extra := indexOf(args, "-cq")
	if extra < 0 {
		t.Fatal("-cq was dropped")
	}
	if profile := indexOf(args, "-profile:v"); extra < profile {
		t.Errorf("extra encoder args came before ours, so ours would override them: %v", args)
	}
	if got := args[len(args)-1]; got != "out.mp4" {
		t.Errorf("the last argument is %q, want the output file: %v", got, args)
	}
}

func TestArgsScalesWhenAsked(t *testing.T) {
	cfg := config.Default()
	cfg.Encoding.Resolution = "1280x720"

	args, err := Args(cfg, monitorTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	line := strings.Join(args, " ")
	if !strings.Contains(line, "scale=1280x720") {
		t.Errorf("the requested resolution is missing: %s", line)
	}
	if !strings.Contains(line, "trunc(iw/2)*2") {
		t.Errorf("the even-dimension guard is missing: %s", line)
	}
}

func TestArgsSourceResolutionAddsNoScale(t *testing.T) {
	cfg := config.Default()
	cfg.Encoding.Resolution = "source"

	args, err := Args(cfg, monitorTarget(), EncoderX264, capture.Capabilities{DDAGrab: map[int]int8{-1: capture.DDAGrabHealthy}}, Output{Path: "out.mp4"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "scale=") {
		t.Errorf("scaling happened for resolution=source: %v", args)
	}
}


func TestOutputDefaults(t *testing.T) {
	cases := []struct {
		name string
		out  Output
		want string
	}{
		{"a file defaults to mp4", Output{Path: "a.mp4"}, "mp4"},
		{"a pipe defaults to raw h264", Output{Pipe: true}, "h264"},
		{"an explicit format wins", Output{Path: "a.mkv", Format: "mkv"}, "mkv"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.out.FormatOrDefault(); got != c.want {
				t.Errorf("FormatOrDefault() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOutputArgsAlwaysGoToStdoutForH264(t *testing.T) {
	// The h264 output has to write to ffmpeg's stdout even when a path was
	// given, because reading that pipe is the entire point of the format.
	args := outputArgs(Output{Pipe: true, Format: "h264"})
	if args[len(args)-1] != "-" {
		t.Errorf("h264 output args = %v, want them to end with -", args)
	}
}

func TestResolutionScale(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"source", ""},
		{"SOURCE", ""},
		{" source ", ""},
		{"1920x1080", "1920x1080"},
		{"1280X720", "1280x720"},
	}
	for _, c := range cases {
		if got := resolutionScale(c.in); got != c.want {
			t.Errorf("resolutionScale(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFFmpegLogLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"debug", "info"},
		{"info", "warning"},
		{"warn", "warning"},
		{"", "warning"},
		{"error", "error"},
	}
	for _, c := range cases {
		if got := ffmpegLogLevel(c.in); got != c.want {
			t.Errorf("ffmpegLogLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestForSetting(t *testing.T) {
	t.Run("auto offers every candidate in order", func(t *testing.T) {
		got, err := ForSetting(config.EncoderAuto)
		if err != nil {
			t.Fatalf("ForSetting: %v", err)
		}
		if len(got) != len(PreferenceOrder) {
			t.Fatalf("got %d candidates, want %d", len(got), len(PreferenceOrder))
		}
		if got[0].Name != "h264_nvenc" {
			t.Errorf("first candidate = %s, want h264_nvenc", got[0].Name)
		}
		if got[len(got)-1].Name != "libx264" {
			t.Errorf("last candidate = %s, want libx264 so software is the last resort", got[len(got)-1].Name)
		}
	})

	t.Run("an explicit choice offers exactly that encoder", func(t *testing.T) {
		got, err := ForSetting(config.EncoderQSV)
		if err != nil {
			t.Fatalf("ForSetting: %v", err)
		}
		if len(got) != 1 || got[0].Name != "h264_qsv" {
			t.Fatalf("got %v, want only h264_qsv", got)
		}
	})

	t.Run("an unknown setting is an error", func(t *testing.T) {
		if _, err := ForSetting("magic"); err == nil {
			t.Fatal("an unknown encoder setting was accepted")
		}
	})
}

func TestLookup(t *testing.T) {
	for _, want := range PreferenceOrder {
		got, ok := Lookup(want.Label)
		if !ok || got.Name != want.Name {
			t.Errorf("Lookup(%q) = %v, %v", want.Label, got, ok)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup invented an encoder")
	}
}

func TestEncoderString(t *testing.T) {
	if got := EncoderNVENC.String(); !strings.Contains(got, "hardware") {
		t.Errorf("EncoderNVENC.String() = %q, want it to say hardware", got)
	}
	if got := EncoderX264.String(); !strings.Contains(got, "software") {
		t.Errorf("EncoderX264.String() = %q, want it to say software", got)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encoder-cache.json")

	// Nothing cached yet.
	if _, ok := loadCacheFrom(path, "v1"); ok {
		t.Fatal("an empty cache reported a hit")
	}

	if err := saveCacheTo(path, cacheEntry{FFmpegVersion: "v1", Encoder: config.EncoderNVENC, EncoderName: "h264_nvenc", ProbedAt: time.Now()}); err != nil {
		t.Fatalf("saveCacheTo: %v", err)
	}

	got, ok := loadCacheFrom(path, "v1")
	if !ok {
		t.Fatal("the cache did not load")
	}
	if got.Name != "h264_nvenc" {
		t.Errorf("cached encoder = %q, want h264_nvenc", got.Name)
	}

	// A different ffmpeg must invalidate the entry: a new build may have fixed
	// or broken an encoder.
	if _, ok := loadCacheFrom(path, "v2"); ok {
		t.Error("the cache survived an ffmpeg version change")
	}
}

func TestCacheIgnoresGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encoder-cache.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A corrupt cache is not worth reporting: it just means probing again.
	if _, ok := loadCacheFrom(path, "v1"); ok {
		t.Error("a corrupt cache reported a hit")
	}
}

func TestCacheRejectsUnknownEncoder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encoder-cache.json")
	if err := saveCacheTo(path, cacheEntry{FFmpegVersion: "v1", Encoder: "obsolete"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCacheFrom(path, "v1"); ok {
		t.Error("an encoder that no longer exists was accepted from the cache")
	}
}

func TestStripProgressKeepsTheMessages(t *testing.T) {
	// This is the shape of real ffmpeg output: progress updates redrawn in
	// place with carriage returns, then messages separated by newlines. This
	// exact line was produced by a real capture during development.
	in := "frame=  1\rframe=  2\rframe=  3\r" +
		"[in#0/gdigrab @ 0x1] Stream #0: not enough frames to estimate rate\n" +
		"Conversion failed!\n"
	got := stripProgress(in)

	if !strings.Contains(got, "not enough frames to estimate rate") {
		t.Errorf("stripProgress = %q, want the message that shared a line with the progress redraw", got)
	}
	if !strings.Contains(got, "Conversion failed!") {
		t.Errorf("stripProgress = %q, want the following message too", got)
	}
	if strings.Contains(got, "frame=  3") {
		t.Errorf("stripProgress = %q, want the progress redraw dropped", got)
	}
}

func TestErrorWithOutput(t *testing.T) {
	base := errors.New("exit status 1")

	t.Run("no output means no decoration", func(t *testing.T) {
		if got := errorWithOutput(base, "   \n  "); !errors.Is(got, base) {
			t.Errorf("errorWithOutput = %v, want the original error", got)
		}
	})

	t.Run("the message survives and the cause is wrapped", func(t *testing.T) {
		got := errorWithOutput(base, "warning: noise\n[h264_nvenc @ 0x1] Cannot load nvcuda.dll\nConversion failed\n")
		if !errors.Is(got, base) {
			t.Errorf("errorWithOutput lost the cause: %v", got)
		}
		if !strings.Contains(got.Error(), "Cannot load nvcuda.dll") {
			t.Errorf("errorWithOutput = %q, want ffmpeg's explanation", got)
		}
	})
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := newTailBuffer(10)
	if _, err := tb.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tb.String(); got != "6789ABCDEF" {
		t.Errorf("tailBuffer = %q, want the last 10 bytes", got)
	}
}

func TestTailBufferAcrossWrites(t *testing.T) {
	tb := newTailBuffer(8)
	for _, s := range []string{"abc", "def", "ghi"} {
		if _, err := tb.Write([]byte(s)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := tb.String(); got != "bcdefghi" {
		t.Errorf("tailBuffer = %q, want bcdefghi", got)
	}
}

func TestFindRejectsAMissingPath(t *testing.T) {
	_, err := Find(context.Background(), filepath.Join(t.TempDir(), "nope", "ffmpeg.exe"))
	if err == nil {
		t.Fatal("Find accepted a path that does not exist")
	}
	if !strings.Contains(err.Error(), "ffmpegPath") {
		t.Errorf("error %q does not name the setting the user has to fix", err)
	}
}

func TestProbeDDAGrabOutputParsesLuma(t *testing.T) {
	// Mock ffmpeg output with signalstats showing healthy luma values
	out := `[Parsed_signalstats_1 @ 0x1] YAVG: 120.5
[Parsed_signalstats_1 @ 0x1] YAVG: 118.2
[Parsed_signalstats_1 @ 0x1] YAVG: 122.1
`
	if !hasMeanLumaAbove(out, 100.0) {
		t.Error("should detect luma > 100")
	}
	if hasMeanLumaAbove(out, 130.0) {
		t.Error("should not detect luma > 130")
	}
}

func TestProbeDDAGrabOutputDetectsBlack(t *testing.T) {
	// Mock ffmpeg output with signalstats showing black frames (YAVG=0)
	out := `[Parsed_signalstats_1 @ 0x1] YAVG: 0.0
[Parsed_signalstats_1 @ 0x1] YAVG: 0.0
[Parsed_signalstats_1 @ 0x1] YAVG: 0.0
`
	if hasMeanLumaAbove(out, 5.0) {
		t.Error("black frames (YAVG=0) should fail threshold")
	}
}

func TestProbeDDAGrabOutputHandlesMissingSignalstats(t *testing.T) {
	// Mock ffmpeg output without signalstats (e.g., error case)
	out := `frame=  1 fps=0.0 q=0.0 size=       0kB time=00:00:00.03 bitrate=   0.0kbits/s
Conversion failed!
`
	if hasMeanLumaAbove(out, 5.0) {
		t.Error("missing signalstats should fail threshold")
	}
}

func TestProbeDDAGrabOutputHandlesMixedFrames(t *testing.T) {
	// Mixed: first two frames black, third frame has content
	out := `[Parsed_signalstats_1 @ 0x1] YAVG: 0.0
[Parsed_signalstats_1 @ 0x1] YAVG: 0.0
[Parsed_signalstats_1 @ 0x1] YAVG: 85.3
`
	if !hasMeanLumaAbove(out, 50.0) {
		t.Error("should detect luma > 50 from the third frame")
	}
}

func TestJoinErrors(t *testing.T) {
	if got := joinErrors(nil); got == nil {
		t.Error("joinErrors(nil) = nil, want an error")
	}
	if got := joinErrors([]error{errors.New("only")}); got.Error() != "only" {
		t.Errorf("joinErrors of one = %q, want it passed through", got)
	}
	got := joinErrors([]error{errors.New("first"), errors.New("second")})
	if !strings.Contains(got.Error(), "first") || !strings.Contains(got.Error(), "second") {
		t.Errorf("joinErrors lost a cause: %q", got)
	}
}

func TestNoEncoderErrorNamesTheSetting(t *testing.T) {
	t.Run("explicit setting", func(t *testing.T) {
		got := noEncoderError(config.EncoderNVENC, []error{errors.New("no capable devices")})
		if !strings.Contains(got.Error(), "encoding.encoder") {
			t.Errorf("error %q does not point at the setting", got)
		}
		if !strings.Contains(got.Error(), "x264") {
			t.Errorf("error %q does not suggest a way out", got)
		}
	})

	t.Run("auto", func(t *testing.T) {
		got := noEncoderError(config.EncoderAuto, []error{errors.New("no capable devices")})
		if !strings.Contains(got.Error(), "no capable devices") {
			t.Errorf("error %q does not include the underlying reasons", got)
		}
	})
}

// indexOf returns the position of a flag in an argument list, or -1.
func indexOf(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}
