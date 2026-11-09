package sound

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestMaterializeWritesEmbeddedCues(t *testing.T) {
	// Keep the test out of the real user cache.
	t.Setenv("LocalAppData", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	for _, name := range []string{"start.mp3", "stop.mp3", "join.mp3", "leave.mp3"} {
		t.Run(name, func(t *testing.T) {
			path, err := materialize(name)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read materialized cue: %v", err)
			}
			want, err := assets.ReadFile("assets/" + name)
			if err != nil {
				t.Fatalf("read embedded cue: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("materialized cue differs from the embedded file")
			}
		})
	}
}

func TestClaimCueCoalescesBursts(t *testing.T) {
	now := time.Now()
	if !claimCue("coalesce-test", now) {
		t.Fatal("first cue was suppressed")
	}
	if claimCue("coalesce-test", now.Add(viewerCueGap/2)) {
		t.Fatal("cue inside the gap was not suppressed")
	}
	if !claimCue("coalesce-test", now.Add(viewerCueGap)) {
		t.Fatal("cue after the gap was suppressed")
	}
}
