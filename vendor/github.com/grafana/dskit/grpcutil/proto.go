package grpcutil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
)

const messageSizeLargerErrFmt = "received message larger than max (%d vs %d)"

// CompressionType for encoding and decoding requests and responses.
type CompressionType int

// Values for CompressionType
const (
	NoCompression CompressionType = iota
	RawSnappy
)

// ParseProtoReader parses a compressed proto from an io.Reader.
func ParseProtoReader(ctx context.Context, reader io.Reader, expectedSize, maxSize int, req proto.Message, compression CompressionType) error {
	sp := opentracing.SpanFromContext(ctx)
	if sp != nil {
		sp.LogFields(otlog.String("event", "util.ParseProtoRequest[start reading]"))
	}
	body, err := decompressRequest(reader, expectedSize, maxSize, compression, sp)
	if err != nil {
		return err
	}

	if sp != nil {
		sp.LogFields(otlog.String("event", "util.ParseProtoRequest[unmarshal]"), otlog.Int("size", len(body)))
	}

	// We re-implement proto.Unmarshal here as it calls XXX_Unmarshal first,
	// which we can't override without upsetting golint.
	req.Reset()
	if u, ok := req.(proto.Unmarshaler); ok {
		err = u.Unmarshal(body)
	} else {
		err = proto.NewBuffer(body).Unmarshal(req)
	}
	if err != nil {
		return err
	}

	return nil
}

func decompressRequest(reader io.Reader, expectedSize, maxSize int, compression CompressionType, sp opentracing.Span) (body []byte, err error) {
	defer func() {
		if err != nil && len(body) > maxSize {
			err = fmt.Errorf(messageSizeLargerErrFmt, len(body), maxSize)
		}
	}()
	if expectedSize > maxSize {
		return nil, fmt.Errorf(messageSizeLargerErrFmt, expectedSize, maxSize)
	}
	buffer, ok := tryBufferFromReader(reader)
	if ok {
		body, err = decompressFromBuffer(buffer, maxSize, compression, sp)
		return
	}
	body, err = decompressFromReader(reader, expectedSize, maxSize, compression, sp)
	return
}

func decompressFromReader(reader io.Reader, expectedSize, maxSize int, compression CompressionType, sp opentracing.Span) ([]byte, error) {
	var (
		buf  bytes.Buffer
		body []byte
		err  error
	)
	if expectedSize > 0 {
		buf.Grow(expectedSize + bytes.MinRead) // extra space guarantees no reallocation
	}
	// Read from LimitReader with limit max+1. So if the underlying
	// reader is over limit, the result will be bigger than max.
	reader = io.LimitReader(reader, int64(maxSize)+1)
	switch compression {
	case NoCompression:
		_, err = buf.ReadFrom(reader)
		body = buf.Bytes()
	case RawSnappy:
		_, err = buf.ReadFrom(reader)
		if err != nil {
			return nil, err
		}
		body, err = decompressFromBuffer(&buf, maxSize, RawSnappy, sp)
	}
	return body, err
}

func decompressFromBuffer(buffer *bytes.Buffer, maxSize int, compression CompressionType, sp opentracing.Span) ([]byte, error) {
	if len(buffer.Bytes()) > maxSize {
		return nil, fmt.Errorf(messageSizeLargerErrFmt, len(buffer.Bytes()), maxSize)
	}
	switch compression {
	case NoCompression:
		return buffer.Bytes(), nil
	case RawSnappy:
		if sp != nil {
			sp.LogFields(otlog.String("event", "util.ParseProtoRequest[decompress]"),
				otlog.Int("size", len(buffer.Bytes())))
		}
		size, err := snappy.DecodedLen(buffer.Bytes())
		if err != nil {
			return nil, err
		}
		if size > maxSize {
			return nil, fmt.Errorf(messageSizeLargerErrFmt, size, maxSize)
		}
		body, err := snappy.Decode(nil, buffer.Bytes())
		if err != nil {
			return nil, err
		}
		return body, nil
	}
	return nil, nil
}

// tryBufferFromReader attempts to cast the reader to a `*bytes.Buffer` this is possible when using httpgrpc.
// If it fails it will return nil and false.
func tryBufferFromReader(reader io.Reader) (*bytes.Buffer, bool) {
	if bufReader, ok := reader.(interface {
		BytesBuffer() *bytes.Buffer
	}); ok && bufReader != nil {
		return bufReader.BytesBuffer(), true
	}
	return nil, false
}

// SerializeProtoResponse serializes a protobuf response into an HTTP response.
func SerializeProtoResponse(w http.ResponseWriter, resp proto.Message, compression CompressionType) error {
	data, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return fmt.Errorf("error marshaling proto response: %v", err)
	}

	switch compression {
	case NoCompression:
	case RawSnappy:
		data = snappy.Encode(nil, data)
	}

	if _, err := w.Write(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return fmt.Errorf("error sending proto response: %v", err)
	}
	return nil
}