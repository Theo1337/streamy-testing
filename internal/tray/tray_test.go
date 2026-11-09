package tray

import "testing"

// The menu must describe what the app is actually doing. Phase 2 writes a file,
// so offering to "Start sharing" would be a small lie, and phase 3 should be
// able to change the wording by changing the session label alone.
func TestActionWord(t *testing.T) {
	for _, tc := range []struct{ name, active, want string }{
		{"nothing running yet", "", "sharing"},
		{"recording to a file", "Recording", "recording"},
		{"streaming to viewers (phase 3)", "Streaming", "streaming"},
		{"whatever case the pipeline used", "RECORDING", "recording"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionWord(tc.active); got != tc.want {
				t.Errorf("actionWord(%q) = %q, want %q", tc.active, got, tc.want)
			}
		})
	}
}

func TestStatusText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status Status
		word   string
		want   string
	}{
		{
			name:   "the controller's own text wins",
			status: Status{Text: "Starting\u2026"},
			word:   "recording",
			want:   "Starting\u2026",
		},
		{
			name:   "idle",
			status: Status{},
			word:   "sharing",
			want:   "",
		},
		{
			name:   "idle, but the pipeline calls it recording",
			status: Status{},
			word:   "recording",
			want:   "",
		},
		{
			name:   "recording, from the pipeline's label",
			status: Status{Sharing: true, Active: "Recording"},
			word:   "recording",
			want:   "Recording",
		},
		{
			name:   "active with no label to go on",
			status: Status{Sharing: true},
			word:   "sharing",
			want:   "Sharing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusText(tc.status, tc.word); got != tc.want {
				t.Errorf("statusText(%+v, %q) = %q, want %q", tc.status, tc.word, got, tc.want)
			}
		})
	}
}
