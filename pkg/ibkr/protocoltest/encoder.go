package protocoltest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
)

// EncodeMessage reproduces the production encodeMsg behavior with a toggle for
// including or omitting the null terminator that follows the binary message type
// in v100+ sessions.
func EncodeMessage(serverVersion int, includeNullAfterType bool, fields ...interface{}) ([]byte, error) {
	var buf bytes.Buffer

	for i, field := range fields {
		if i == 0 && serverVersion >= 100 {
			if err := writeBinaryType(&buf, field, includeNullAfterType); err != nil {
				return nil, err
			}
			continue
		}

		if err := writeASCIIField(&buf, field); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func writeBinaryType(buf *bytes.Buffer, field interface{}, includeNull bool) error {
	var msgType int32

	switch v := field.(type) {
	case int:
		msgType = int32(v)
	case int32:
		msgType = v
	case int64:
		msgType = int32(v)
	default:
		return fmt.Errorf("unsupported message type %T", field)
	}

	if err := binary.Write(buf, binary.BigEndian, msgType); err != nil {
		return err
	}
	if includeNull {
		buf.WriteByte('\x00')
	}
	return nil
}

func writeASCIIField(buf *bytes.Buffer, field interface{}) error {
	switch v := field.(type) {
	case int:
		buf.WriteString(strconv.Itoa(v))
	case int64:
		buf.WriteString(strconv.FormatInt(v, 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(v), 10))
	case float64:
		buf.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	case string:
		buf.WriteString(v)
	case bool:
		if v {
			buf.WriteString("1")
		} else {
			buf.WriteString("0")
		}
	default:
		buf.WriteString(fmt.Sprintf("%v", v))
	}
	buf.WriteByte('\x00')
	return nil
}
