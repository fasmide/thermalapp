package camera

// Pre-computed 18-byte USB commands with CRC.
// Sourced from P3_PROTOCOL.md and p3_camera.py.
// The camera does not verify CRCs, but correct values are included.
var commands = map[string][]byte{
	// Register reads (0x0101 command type)
	"read_name":        hexBytes("0101810001000000000000001e0000004f90"),
	"read_version":     hexBytes("0101810002000000000000000c0000001f63"),
	"read_part_number": hexBytes("01018100060000000000000040000000654f"),
	"read_serial":      hexBytes("01018100070000000000000040000000104c"),
	"read_hw_version":  hexBytes("010181000a00000000000000400000001959"),
	"read_model_long":  hexBytes("010181000f0000000000000040000000b857"),

	// Stream control (0x012f command type)
	"start_stream": hexBytes("012f81000000000000000000010000004930"),
	"gain_low":     hexBytes("012f41000000000000000000000000003c3a"),
	"gain_high":    hexBytes("012f41000100000000000000000000004939"),

	// Shutter (0x0136 command type)
	"shutter": hexBytes("01364300000000000000000000000000cd0b"),
}

// hexBytes converts a hex string to []byte. Panics on invalid input (init-time only).
func hexBytes(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		b[i/2] = hexNibble(s[i])<<4 | hexNibble(s[i+1])
	}

	return b
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		panic("invalid hex nibble: " + string(c))
	}
}
