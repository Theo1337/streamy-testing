package output

import "bytes"

type h264Info struct {
	hasIDR bool
	sps    []byte
	pps    []byte
}

func inspectH264(data []byte) h264Info {
	var info h264Info
	for _, nal := range splitNALs(data) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 5:
			info.hasIDR = true
		case 7:
			info.sps = append([]byte{0, 0, 0, 1}, nal...)
		case 8:
			info.pps = append([]byte{0, 0, 0, 1}, nal...)
		}
	}
	return info
}

// ensureParameterSets makes every IDR self-contained using the most recent
// SPS/PPS seen on this connection. ffmpeg often writes parameter sets only once
// at the start of a stream; a relay that reconnects or a viewer that joins later
// still needs them before the IDR slice.
func ensureParameterSets(accessUnit []byte, info h264Info, sps, pps []byte) []byte {
	if !info.hasIDR {
		return accessUnit
	}
	var prefix []byte
	if info.sps == nil && len(sps) > 0 {
		prefix = append(prefix, sps...)
	}
	if info.pps == nil && len(pps) > 0 {
		prefix = append(prefix, pps...)
	}
	if len(prefix) == 0 {
		return accessUnit
	}
	return insertBeforeIDR(accessUnit, prefix)
}

// insertBeforeIDR places parameter sets immediately before the first IDR NAL,
// after any access-unit delimiter or SEI that must stay in front.
func insertBeforeIDR(data, prefix []byte) []byte {
	if len(prefix) == 0 {
		return data
	}
	for _, part := range splitNALOffsets(data) {
		if len(part.nal) > 0 && part.nal[0]&0x1f == 5 {
			result := make([]byte, 0, len(data)+len(prefix))
			result = append(result, data[:part.start]...)
			result = append(result, prefix...)
			result = append(result, data[part.start:]...)
			return result
		}
	}
	return data
}

func splitNALs(data []byte) [][]byte {
	var result [][]byte
	for _, part := range splitNALOffsets(data) {
		result = append(result, part.nal)
	}
	return result
}

type nalPart struct {
	start int
	nal   []byte
}

func splitNALOffsets(data []byte) []nalPart {
	var starts []int
	var lengths []int
	for i := 0; i+2 < len(data); {
		switch {
		case data[i] == 0 && data[i+1] == 0 && data[i+2] == 1:
			starts = append(starts, i)
			lengths = append(lengths, 3)
			i += 3
		case i+3 < len(data) && bytes.Equal(data[i:i+4], []byte{0, 0, 0, 1}):
			starts = append(starts, i)
			lengths = append(lengths, 4)
			i += 4
		default:
			i++
		}
	}
	parts := make([]nalPart, 0, len(starts))
	for index, start := range starts {
		end := len(data)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		nalStart := start + lengths[index]
		if nalStart <= end {
			parts = append(parts, nalPart{start: start, nal: data[nalStart:end]})
		}
	}
	return parts
}
