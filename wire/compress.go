package wire

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// Compressor ids on the wire (spec 2061 doc 16 §11). doc supports the noop passthrough
// and zlib, both in the standard library. snappy and zstd are defined by the protocol
// but live in external modules, and doc carries no third-party dependencies, so the
// server never negotiates them. A driver that offers only snappy or zstd simply runs
// uncompressed, which every driver supports.
const (
	compressorNoop byte = 0
	compressorZlib byte = 2
)

// compressThreshold is the smallest reply the server bothers to compress. Small replies
// (hello, ping, command acks) cost more in CPU than they save in bytes, so they go out
// uncompressed (spec 2061 doc 16 §11.3).
const compressThreshold = 512

// serverCompressors lists the wire names doc can handle, in server preference order. The
// negotiated compressor is the first client-offered name that appears here.
var serverCompressors = []string{"zlib"}

// negotiateCompressor picks the compressor from the client's offered names, returning the
// chosen id and the names to advertise back in hello. It returns compressorNoop and an
// empty list when there is no overlap.
func negotiateCompressor(offered []string) (byte, []string) {
	for _, want := range serverCompressors {
		for _, have := range offered {
			if have == want {
				return compressorIDFor(want), []string{want}
			}
		}
	}
	return compressorNoop, nil
}

// compressorIDFor maps a wire compressor name to its id.
func compressorIDFor(name string) byte {
	switch name {
	case "zlib":
		return compressorZlib
	default:
		return compressorNoop
	}
}

// parseOpCompressed unwraps an OP_COMPRESSED payload (spec 2061 doc 16 §11.1): the
// original opcode, the uncompressed size, the compressor id, and the compressed message
// body. It decompresses and returns the original opcode and the inner message payload
// (everything that followed the original 16-byte header). The uncompressed size is
// checked against maxBytes before allocation so a hostile size cannot exhaust memory.
func parseOpCompressed(payload []byte, maxBytes int32) (originalOpcode int32, inner []byte, err error) {
	if len(payload) < 9 {
		return 0, nil, fmt.Errorf("%w: OP_COMPRESSED shorter than its header", errProtocol)
	}
	originalOpcode = int32(binary.LittleEndian.Uint32(payload[0:4]))
	uncompressedSize := int32(binary.LittleEndian.Uint32(payload[4:8]))
	compressorID := payload[8]
	body := payload[9:]

	if uncompressedSize < 0 || uncompressedSize > maxBytes {
		return 0, nil, fmt.Errorf("%w: OP_COMPRESSED uncompressed size %d out of range", errProtocol, uncompressedSize)
	}

	inner, err = decompress(compressorID, body, int(uncompressedSize))
	if err != nil {
		return 0, nil, err
	}
	if len(inner) != int(uncompressedSize) {
		return 0, nil, fmt.Errorf("%w: OP_COMPRESSED size mismatch: header %d, got %d", errProtocol, uncompressedSize, len(inner))
	}
	return originalOpcode, inner, nil
}

// decompress reverses a compressor over src, expecting sizeHint bytes out.
func decompress(id byte, src []byte, sizeHint int) ([]byte, error) {
	switch id {
	case compressorNoop:
		out := make([]byte, len(src))
		copy(out, src)
		return out, nil
	case compressorZlib:
		zr, err := zlib.NewReader(bytes.NewReader(src))
		if err != nil {
			return nil, fmt.Errorf("%w: zlib reader: %v", errProtocol, err)
		}
		defer func() { _ = zr.Close() }()
		out := bytes.NewBuffer(make([]byte, 0, sizeHint))
		if _, err := io.Copy(out, zr); err != nil {
			return nil, fmt.Errorf("%w: zlib inflate: %v", errProtocol, err)
		}
		return out.Bytes(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported compressor id %d", errProtocol, id)
	}
}

// compress applies a compressor to src.
func compress(id byte, src []byte) ([]byte, error) {
	switch id {
	case compressorNoop:
		return src, nil
	case compressorZlib:
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(src); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("wire: unsupported compressor id %d", id)
	}
}

// wrapCompressed re-frames an already-encoded reply as OP_COMPRESSED. It reads the inner
// message's opcode and payload (the bytes after the 16-byte header) from the reply and
// rebuilds it under the OP_COMPRESSED envelope (spec 2061 doc 16 §11.1). On any
// compression error it returns the original reply unchanged so the connection still makes
// progress, just uncompressed.
func wrapCompressed(reply []byte, compressorID byte) []byte {
	if len(reply) < headerLen {
		return reply
	}
	requestID := int32(binary.LittleEndian.Uint32(reply[4:8]))
	responseTo := int32(binary.LittleEndian.Uint32(reply[8:12]))
	originalOpcode := int32(binary.LittleEndian.Uint32(reply[12:16]))
	originalPayload := reply[headerLen:]

	compressed, err := compress(compressorID, originalPayload)
	if err != nil {
		return reply
	}

	total := headerLen + 4 + 4 + 1 + len(compressed)
	buf := make([]byte, headerLen+9, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opCompressed))
	binary.LittleEndian.PutUint32(buf[16:20], uint32(originalOpcode))
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(originalPayload)))
	buf[24] = compressorID
	buf = append(buf, compressed...)
	return buf
}
