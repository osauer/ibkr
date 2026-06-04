package ibkr

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

const protoCancelOrderMsgID = cancelOrder + protoBufMsgID

func (c *Connection) encodeCancelOrderMessage(orderID int) ([]byte, error) {
	if err := validateProtoInt32("orderID", orderID); err != nil {
		return nil, err
	}
	if c.serverVersion >= minServerVerProtoBufPlaceOrder {
		return encodeCancelOrderProtoFrame(orderID), nil
	}
	return c.encodeCancelOrderLegacyFrame(orderID), nil
}

func encodeCancelOrderProtoFrame(orderID int) []byte {
	body := encodeCancelOrderProtoBody(orderID)
	msg := make([]byte, 0, 4+len(body))
	msg = binary.BigEndian.AppendUint32(msg, uint32(protoCancelOrderMsgID))
	return append(msg, body...)
}

func encodeCancelOrderProtoBody(orderID int) []byte {
	var body []byte
	body = protoAppendInt32(body, 1, int32(orderID))
	body = protoAppendMessage(body, 2, nil)
	return body
}

func (c *Connection) encodeCancelOrderLegacyFrame(orderID int) []byte {
	fields := []any{cancelOrder}
	if c.serverVersion < minServerVerCMETaggingFields {
		fields = append(fields, 1)
	}
	fields = append(fields, orderID)
	if c.serverVersion >= minServerVerManualOrderTime {
		fields = append(fields, "")
	}
	if c.serverVersion >= minServerVerRFQFields && c.serverVersion < minServerVerUndoRFQFields {
		fields = append(fields, "", "", maxProtoInt32)
	}
	if c.serverVersion >= minServerVerCMETaggingFields {
		fields = append(fields, "", maxProtoInt32)
	}
	return c.encodeMsg(fields...)
}

func summarizeCancelOrderProtoFrame(msgBytes []byte) []string {
	fields := []string{strconv.Itoa(protoCancelOrderMsgID), "protobuf", "base_msg_id=" + strconv.Itoa(cancelOrder)}
	if len(msgBytes) < 4 {
		return append(fields, "decode_error=truncated")
	}
	summary, err := parseCancelOrderProtoSummary(msgBytes[4:])
	if err != nil {
		return append(fields, "decode_error="+err.Error())
	}
	fields = append(fields, "orderId="+strconv.Itoa(summary.orderID))
	if summary.hasOrderCancel {
		fields = append(fields, "orderCancel=true")
	}
	return fields
}

type cancelOrderProtoSummary struct {
	orderID        int
	hasOrderCancel bool
}

func parseCancelOrderProtoSummary(body []byte) (cancelOrderProtoSummary, error) {
	var summary cancelOrderProtoSummary
	err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.orderID = int(v)
		case 2:
			if wireType != protoWireBytes {
				return fmt.Errorf("field_%d_not_message", fieldNumber)
			}
			if err := parseOrderCancelProtoSummary(value); err != nil {
				return err
			}
			summary.hasOrderCancel = true
		}
		return nil
	})
	return summary, err
}

func parseOrderCancelProtoSummary(body []byte) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, _ []byte) error {
		switch fieldNumber {
		case 1, 2:
			if wireType != protoWireBytes {
				return fmt.Errorf("field_%d_not_string", fieldNumber)
			}
		case 3:
			if wireType != protoWireVarint {
				return fmt.Errorf("field_%d_not_varint", fieldNumber)
			}
		default:
			return fmt.Errorf("unsupported_order_cancel_field_%d", fieldNumber)
		}
		return nil
	})
}
