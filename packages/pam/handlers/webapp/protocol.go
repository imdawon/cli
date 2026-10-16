package webapp

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// The viewer and the gateway talk over a byte stream (WebSocket ↔ backend ↔ relay ↔ gateway), so every
// message is framed as [u32 big-endian length][u8 type][payload], where length counts the type byte too.
// The viewer never speaks CDP: this protocol is the whole surface it can reach.

type MessageType uint8

const (
	// Gateway → viewer
	MessageTypeFrame MessageType = 0x01 // payload: [u32 metaLen][FrameMeta JSON][JPEG bytes]
	MessageTypeEvent MessageType = 0x02 // payload: Event JSON

	// Viewer → gateway
	MessageTypeInput MessageType = 0x10 // payload: input JSON
)

// Bounds a single message so a misbehaving peer can't make the gateway allocate without limit. A 1280x800
// JPEG screencast frame is well under 1 MiB.
const maxMessageBytes = 8 << 20

type FrameMeta struct {
	Seq    uint64  `json:"seq"`
	Ts     float64 `json:"ts"`
	Width  int     `json:"w"`
	Height int     `json:"h"`
}

type EventKind string

const (
	EventKindReady     EventKind = "ready"
	EventKindNavigated EventKind = "navigated"
	EventKindEnded     EventKind = "ended"
	EventKindBlocked   EventKind = "blocked"
	EventKindSelect    EventKind = "select"
)

type Event struct {
	Kind   EventKind `json:"kind"`
	URL    string    `json:"url,omitempty"`
	Width  int       `json:"w,omitempty"`
	Height int       `json:"h,omitempty"`
	Reason string    `json:"reason,omitempty"`

	// select: the menu the viewer draws in place of Chrome's, positioned in page CSS pixels
	Options  []SelectOption `json:"options,omitempty"`
	Selected *int           `json:"selected,omitempty"`
	Rect     *SelectRect    `json:"rect,omitempty"`
}

type Message struct {
	Type    MessageType
	Payload []byte
}

func writeMessage(w io.Writer, msgType MessageType, payload []byte) error {
	if len(payload)+1 > maxMessageBytes {
		return fmt.Errorf("message too large: %d bytes", len(payload)+1)
	}
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[:4], uint32(len(payload)+1))
	header[4] = byte(msgType)
	if _, err := w.Write(append(header, payload...)); err != nil {
		return err
	}
	return nil
}

func encodeFrame(meta FrameMeta, jpeg []byte) ([]byte, error) {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 4+len(metaJSON)+len(jpeg))
	binary.BigEndian.PutUint32(payload[:4], uint32(len(metaJSON)))
	copy(payload[4:], metaJSON)
	copy(payload[4+len(metaJSON):], jpeg)
	return payload, nil
}

func readMessage(r io.Reader) (Message, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return Message{}, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > maxMessageBytes {
		return Message{}, fmt.Errorf("invalid message length: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, err
	}
	return Message{Type: MessageType(body[0]), Payload: body[1:]}, nil
}
