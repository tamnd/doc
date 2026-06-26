// Package wire implements the MongoDB wire protocol server for doc (spec 2061 doc 16).
// It frames OP_MSG and the legacy OP_QUERY handshake, dispatches commands against an
// open *doc.DB, and streams cursor results. The server is a thin shell over the library:
// it speaks the wire so existing MongoDB drivers connect unchanged, and every command
// maps to a library call.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/tamnd/doc/bson"
)

// Operation codes the server recognizes (spec 2061 doc 16 §3.2). The server speaks
// OP_MSG for all application traffic, OP_QUERY only for the legacy handshake, and
// OP_COMPRESSED as a wrapper once compression is negotiated.
const (
	opReply      = 1
	opQuery      = 2004
	opCompressed = 2012
	opMsg        = 2013
)

// headerLen is the fixed 16-byte MsgHeader on every wire message.
const headerLen = 16

// DefaultMaxMessageBytes is the largest message the server accepts, matching MongoDB's
// 48 MiB default (spec 2061 doc 16 §3.1).
const DefaultMaxMessageBytes = 48 * 1024 * 1024

// OP_MSG flag bits (spec 2061 doc 16 §3.3). exhaustAllowed (bit 16) is read by the
// getMore path in M8-b.
const (
	flagChecksumPresent = 1 << 0
	flagMoreToCome      = 1 << 1
)

// castagnoli is the CRC-32C table used for the optional OP_MSG checksum, the same
// polynomial MongoDB uses.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errProtocol marks a framing or protocol violation. The connection loop turns it into
// a ProtocolError reply where it can, and otherwise drops the connection.
var errProtocol = errors.New("wire: protocol error")

// header is the 16-byte MsgHeader that prefixes every message.
type header struct {
	MessageLength int32
	RequestID     int32
	ResponseTo    int32
	OpCode        int32
}

// rawMessage is a framed message: its header plus the payload after the header.
type rawMessage struct {
	header  header
	payload []byte
}

// readMessage reads one framed message, enforcing the maximum length so a hostile or
// corrupt client cannot ask the server to allocate an unbounded buffer.
func readMessage(r io.Reader, maxBytes int32) (*rawMessage, error) {
	var hb [headerLen]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return nil, err
	}
	h := header{
		MessageLength: int32(binary.LittleEndian.Uint32(hb[0:4])),
		RequestID:     int32(binary.LittleEndian.Uint32(hb[4:8])),
		ResponseTo:    int32(binary.LittleEndian.Uint32(hb[8:12])),
		OpCode:        int32(binary.LittleEndian.Uint32(hb[12:16])),
	}
	if h.MessageLength < headerLen {
		return nil, fmt.Errorf("%w: message length %d below header size", errProtocol, h.MessageLength)
	}
	if h.MessageLength > maxBytes {
		return nil, fmt.Errorf("%w: message length %d over limit %d", errProtocol, h.MessageLength, maxBytes)
	}
	payload := make([]byte, h.MessageLength-headerLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return &rawMessage{header: h, payload: payload}, nil
}

// opMsgIn is a decoded request OP_MSG: the body command document plus any kind-1
// document sequences keyed by identifier.
type opMsgIn struct {
	flags     uint32
	body      bson.Raw
	sequences map[string][]bson.Raw
}

// checksumRequested reports whether the client set the checksumPresent flag, which the
// server mirrors on its reply.
func (m *opMsgIn) checksumRequested() bool { return m.flags&flagChecksumPresent != 0 }

// parseOpMsg decodes an OP_MSG payload into its body and document sequences. It rejects
// a message with no body section, more than one body, or an unknown section kind. A
// moreToCome flag on a request marks it fire-and-forget; the connection loop reads the
// flag off the parsed message and skips the reply.
func parseOpMsg(payload []byte) (*opMsgIn, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("%w: OP_MSG shorter than flag bits", errProtocol)
	}
	flags := binary.LittleEndian.Uint32(payload[0:4])
	sections := payload[4:]
	if flags&flagChecksumPresent != 0 {
		if len(sections) < 4 {
			return nil, fmt.Errorf("%w: checksum flag set but no room for checksum", errProtocol)
		}
		// The trailing 4 bytes are the CRC-32C; the sections end before it.
		sections = sections[:len(sections)-4]
	}

	msg := &opMsgIn{flags: flags}
	for len(sections) > 0 {
		kind := sections[0]
		sections = sections[1:]
		switch kind {
		case 0:
			if msg.body != nil {
				return nil, fmt.Errorf("%w: OP_MSG has more than one body section", errProtocol)
			}
			doc, rest, err := takeDocument(sections)
			if err != nil {
				return nil, err
			}
			msg.body = doc
			sections = rest
		case 1:
			rest, err := parseDocSequence(sections, msg)
			if err != nil {
				return nil, err
			}
			sections = rest
		default:
			return nil, fmt.Errorf("%w: unknown OP_MSG section kind %d", errProtocol, kind)
		}
	}
	if msg.body == nil {
		return nil, fmt.Errorf("%w: OP_MSG has no body section", errProtocol)
	}
	return msg, nil
}

// takeDocument reads one self-delimiting BSON document off the front of b and returns it
// with the remaining bytes. BSON documents start with their own 4-byte little-endian
// length, so the reader locates the next section without a separator.
func takeDocument(b []byte) (bson.Raw, []byte, error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("%w: truncated BSON length", errProtocol)
	}
	n := int(int32(binary.LittleEndian.Uint32(b[0:4])))
	if n < bson.MinDocLen || n > len(b) {
		return nil, nil, fmt.Errorf("%w: BSON document length %d out of range", errProtocol, n)
	}
	return bson.Raw(b[:n]), b[n:], nil
}

// parseDocSequence reads a kind-1 section: a length, a cstring identifier, and a run of
// BSON documents. It routes the documents into msg.sequences under the identifier.
func parseDocSequence(b []byte, msg *opMsgIn) ([]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: truncated document sequence size", errProtocol)
	}
	size := int(int32(binary.LittleEndian.Uint32(b[0:4])))
	if size < 4 || size > len(b) {
		return nil, fmt.Errorf("%w: document sequence size %d out of range", errProtocol, size)
	}
	section := b[4:size]
	rest := b[size:]

	id, docsBytes, err := takeCString(section)
	if err != nil {
		return nil, err
	}
	var docs []bson.Raw
	for len(docsBytes) > 0 {
		doc, more, err := takeDocument(docsBytes)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
		docsBytes = more
	}
	if msg.sequences == nil {
		msg.sequences = make(map[string][]bson.Raw)
	}
	msg.sequences[id] = append(msg.sequences[id], docs...)
	return rest, nil
}

// takeCString reads a null-terminated UTF-8 string off the front of b.
func takeCString(b []byte) (string, []byte, error) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], nil
		}
	}
	return "", nil, fmt.Errorf("%w: unterminated cstring", errProtocol)
}

// encodeOpMsgReply frames a single-body OP_MSG reply. It mirrors the client's checksum
// choice: when withChecksum is set it appends a CRC-32C over the whole message.
func encodeOpMsgReply(requestID, responseTo int32, body bson.Raw, withChecksum bool) []byte {
	var flags uint32
	if withChecksum {
		flags |= flagChecksumPresent
	}
	bodyLen := len(body)
	total := headerLen + 4 + 1 + bodyLen
	if withChecksum {
		total += 4
	}
	buf := make([]byte, headerLen+4+1, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opMsg))
	binary.LittleEndian.PutUint32(buf[16:20], flags)
	buf[20] = 0 // kind 0 body section
	buf = append(buf, body...)
	if withChecksum {
		sum := crc32.Checksum(buf, castagnoli)
		var sb [4]byte
		binary.LittleEndian.PutUint32(sb[:], sum)
		buf = append(buf, sb[:]...)
	}
	return buf
}

// parseOpQuery decodes the legacy OP_QUERY used only for the handshake. It returns the
// collection name and the query document. The server accepts OP_QUERY solely for
// hello/isMaster on admin.$cmd (spec 2061 doc 16 §3.4).
func parseOpQuery(payload []byte) (collection string, query bson.Raw, err error) {
	if len(payload) < 4 {
		return "", nil, fmt.Errorf("%w: OP_QUERY shorter than flags", errProtocol)
	}
	// flags(4), fullCollectionName cstring, numberToSkip(4), numberToReturn(4), query doc
	rest := payload[4:]
	collection, rest, err = takeCString(rest)
	if err != nil {
		return "", nil, err
	}
	if len(rest) < 8 {
		return "", nil, fmt.Errorf("%w: OP_QUERY missing skip/return", errProtocol)
	}
	rest = rest[8:]
	query, _, err = takeDocument(rest)
	if err != nil {
		return "", nil, err
	}
	return collection, query, nil
}

// encodeOpReply frames a legacy OP_REPLY carrying exactly one document, the answer to a
// handshake OP_QUERY (spec 2061 doc 16 §3.4).
func encodeOpReply(requestID, responseTo int32, doc bson.Raw) []byte {
	// responseFlags(4), cursorID(8), startingFrom(4), numberReturned(4), document
	const fixed = 4 + 8 + 4 + 4
	total := headerLen + fixed + len(doc)
	buf := make([]byte, headerLen+fixed, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opReply))
	// responseFlags 0, cursorID 0, startingFrom 0, numberReturned 1
	binary.LittleEndian.PutUint32(buf[16:20], 0)
	binary.LittleEndian.PutUint64(buf[20:28], 0)
	binary.LittleEndian.PutUint32(buf[28:32], 0)
	binary.LittleEndian.PutUint32(buf[32:36], 1)
	buf = append(buf, doc...)
	return buf
}
