package protocoltest

import (
	"bytes"
	"encoding/binary"
)

// HandshakeFrame constructs the initial IBKR API handshake frame for a given
// version descriptor (e.g., "v151..151").
func HandshakeFrame(descriptor string) []byte {
	descriptorBytes := append([]byte(descriptor), '\x00')

	var frame bytes.Buffer
	frame.Grow(4 + 4 + len(descriptorBytes))
	frame.WriteString("API\x00")

	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(descriptorBytes)))
	frame.Write(lengthBuf[:])
	frame.Write(descriptorBytes)

	return frame.Bytes()
}
