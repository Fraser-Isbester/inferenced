// Package bedrocktest builds AWS event-stream frames for tests.
package bedrocktest

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"sort"
)

// Frame encodes one event-stream message with string headers.
func Frame(headers map[string]string, payload []byte) []byte {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hb []byte
	for _, k := range keys {
		v := headers[k]
		hb = append(hb, byte(len(k)))
		hb = append(hb, k...)
		hb = append(hb, 7)
		hb = binary.BigEndian.AppendUint16(hb, uint16(len(v)))
		hb = append(hb, v...)
	}
	total := 12 + len(hb) + len(payload) + 4
	out := binary.BigEndian.AppendUint32(nil, uint32(total))
	out = binary.BigEndian.AppendUint32(out, uint32(len(hb)))
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	out = append(out, hb...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

// Event is an event message of the given type.
func Event(eventType string, payload []byte) []byte {
	return Frame(map[string]string{
		":message-type": "event", ":event-type": eventType, ":content-type": "application/json",
	}, payload)
}

// Chunk is an InvokeModelWithResponseStream chunk wrapping the model's own
// event JSON.
func Chunk(modelEvent string) []byte {
	b, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(modelEvent))})
	return Event("chunk", b)
}
