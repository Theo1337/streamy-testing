package protocol

import "time"

const (
	// ProtocolVersion is the Activity protocol version
	ProtocolVersion = 1

	// Media header: 1 byte type, 1 byte flags, 8 bytes timestamp (Unix ms)
	MediaHeaderSize = 10

	MediaVideo = 0x01
	MediaAudio = 0x02

	FlagKeyframe = 0x01

	MaxControlBytes = 16 << 10  // 16 KB
	MaxMediaBytes   = 8 << 20   // 8 MB

	CloseHostGone     = 4001
	CloseRoomInUse    = 4002
	CloseRoomFull     = 4003
	CloseUnauthorized = 4008

	DefaultMaxViewers = 200
	MaxRoomIDLength   = 128
	MinRoomIDLength   = 1

	ReadDeadline = 70 * time.Second
)

type MediaFrame struct {
	Kind      byte
	Flags     byte
	Timestamp uint64
	Payload   []byte
}

func EncodeMediaFrame(kind, flags byte, timestamp uint64, payload []byte) []byte {
	frame := make([]byte, MediaHeaderSize+len(payload))
	frame[0] = kind
	frame[1] = flags
	// Big-endian timestamp
	for i := 0; i < 8; i++ {
		frame[2+i] = byte(timestamp >> (56 - i*8))
	}
	copy(frame[MediaHeaderSize:], payload)
	return frame
}

func ParseMediaFrame(data []byte) (MediaFrame, error) {
	if len(data) < MediaHeaderSize {
		return MediaFrame{}, ErrBadFrame
	}
	if data[0] != MediaVideo && data[0] != MediaAudio {
		return MediaFrame{}, ErrBadFrame
	}
	var timestamp uint64
	for i := 0; i < 8; i++ {
		timestamp = (timestamp << 8) | uint64(data[2+i])
	}
	return MediaFrame{
		Kind:      data[0],
		Flags:     data[1],
		Timestamp: timestamp,
		Payload:   data[MediaHeaderSize:],
	}, nil
}

var ErrBadFrame = &protocolError{"malformed media frame"}

type protocolError struct {
	msg string
}

func (e *protocolError) Error() string { return e.msg }