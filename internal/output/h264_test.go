package output

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEnsureParameterSetsPrefixesIDR(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f}
	pps := []byte{0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80}
	accessUnit := []byte{0, 0, 0, 1, 0x65, 0x80, 0x80}

	got := ensureParameterSets(accessUnit, inspectH264(accessUnit), sps, pps)
	if !bytes.Contains(got, sps) || !bytes.Contains(got, pps) {
		t.Fatalf("IDR is missing cached parameter sets: %x", got)
	}
	if !bytes.HasSuffix(got, accessUnit) {
		t.Fatalf("IDR slice moved or changed: %x", got)
	}
}

func TestEnsureParameterSetsLeavesCompleteAccessUnitAlone(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f}
	pps := []byte{0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80}
	accessUnit := append(append(append([]byte{}, sps...), pps...), 0, 0, 0, 1, 0x65, 0x80)

	got := ensureParameterSets(accessUnit, inspectH264(accessUnit), sps, pps)
	if !bytes.Equal(got, accessUnit) {
		t.Fatalf("complete access unit was changed:\n got %x\nwant %x", got, accessUnit)
	}
}

func TestEnsureParameterSetsIgnoresDeltaFrames(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f}
	pps := []byte{0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80}
	accessUnit := []byte{0, 0, 0, 1, 0x41, 0x80}

	got := ensureParameterSets(accessUnit, inspectH264(accessUnit), sps, pps)
	if !bytes.Equal(got, accessUnit) {
		t.Fatalf("delta frame was changed: %x", got)
	}
}

func TestInspectH264(t *testing.T) {
	payload := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, // SPS
		0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, // PPS
		0, 0, 0, 1, 0x65, 0x80, // IDR
	}
	info := inspectH264(payload)
	if !info.hasIDR || info.sps == nil || info.pps == nil {
		t.Fatalf("inspectH264 = %+v, want IDR with SPS/PPS", info)
	}
}

func TestRelayPublishURL(t *testing.T) {
	got, err := relayPublishURL("wss://relay.example.com/base/", "12345")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://relay.example.com/base/v1/publish/12345" {
		t.Fatalf("relayPublishURL = %q", got)
	}
	if _, err := relayPublishURL("https://relay.example.com", "12345"); err == nil {
		t.Fatal("https URL should be rejected")
	}
}

func TestEncodeRelayFrame(t *testing.T) {
	frame := outbound{kind: relayVideo, flags: relayKeyframe, timestamp: 1234, payload: []byte{1, 2, 3}}
	data := encodeRelayFrame(frame)
	if len(data) != relayHeaderSize+3 {
		t.Fatalf("frame length = %d", len(data))
	}
	if data[0] != relayVideo || data[1] != relayKeyframe {
		t.Fatalf("header = %x", data[:2])
	}
	if got := binary.BigEndian.Uint64(data[2:10]); got != 1234 {
		t.Fatalf("timestamp = %d, want 1234", got)
	}
}
