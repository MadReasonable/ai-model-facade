package backend

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

const (
	// maxSSELineBytes caps one line from the backend stream; a single chat
	// completion chunk is far smaller.
	maxSSELineBytes = 1 << 20
	// maxSSEEventBytes caps the joined data payload of one event.
	maxSSEEventBytes = 8 << 20
)

// StreamError is an `event: error` event from the backend; Data is its payload.
type StreamError struct {
	Data []byte
}

func (e *StreamError) Error() string {
	return "backend error event: " + string(e.Data)
}

// SSEStream reads an OpenAI-style event stream: it yields each event's data
// payload, ends with io.EOF at `data: [DONE]`, and reports io.ErrUnexpectedEOF
// if the body ends without it. An `event: error` event is returned as a
// *StreamError. An event left unterminated when the body ends is discarded,
// as the SSE spec requires.
type SSEStream struct {
	body     io.ReadCloser
	reader   *bufio.Reader
	done     bool
	lastLine []byte // last discarded line, kept as context for truncation errors
}

func NewSSEStream(body io.ReadCloser) *SSEStream {
	return &SSEStream{body: body, reader: bufio.NewReaderSize(body, 16<<10)}
}

func (s *SSEStream) Next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	var data []byte
	hasData := false
	eventType := ""
	for {
		line, err := s.readLine()
		if err == io.EOF {
			return nil, s.unexpectedEOF()
		}
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if hasData {
				return s.dispatch(eventType, data)
			}
			eventType = ""
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if len(data)+len(value) > maxSSEEventBytes {
				return nil, fmt.Errorf("backend event exceeds %d bytes", maxSSEEventBytes)
			}
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
		case "event":
			eventType = string(value)
		case "": // comment line (leading ':'), e.g. keepalives
		default: // unknown fields are ignored per the SSE spec, but remembered
			s.remember(line)
		}
	}
}

func (s *SSEStream) dispatch(eventType string, data []byte) ([]byte, error) {
	if eventType == "error" {
		return nil, &StreamError{Data: data}
	}
	if string(data) == "[DONE]" {
		s.done = true
		return nil, io.EOF
	}
	return data, nil
}

// readLine returns one line including its newline, erroring on lines over
// maxSSELineBytes. A partial line at EOF is discarded per the SSE spec.
func (s *SSEStream) readLine() ([]byte, error) {
	chunk, err := s.reader.ReadSlice('\n')
	if err == nil {
		return chunk, nil
	}
	line := append([]byte(nil), chunk...)
	for {
		if len(line) > maxSSELineBytes {
			return nil, fmt.Errorf("backend stream line exceeds %d bytes", maxSSELineBytes)
		}
		switch err {
		case nil:
			return line, nil
		case bufio.ErrBufferFull:
		case io.EOF:
			if len(line) > 0 {
				s.remember(line)
			}
			return nil, io.EOF
		default:
			return nil, err
		}
		chunk, err = s.reader.ReadSlice('\n')
		line = append(line, chunk...)
	}
}

// unexpectedEOF reports a stream that ended without [DONE]. The last line
// that didn't parse as an event is included: llama-server and some proxies
// write a bare JSON error before closing, and that is the useful part.
func (s *SSEStream) unexpectedEOF() error {
	if len(s.lastLine) > 0 {
		return fmt.Errorf("%w; last unparsed line: %s", io.ErrUnexpectedEOF, s.lastLine)
	}
	return io.ErrUnexpectedEOF
}

func (s *SSEStream) remember(line []byte) {
	const max = 256
	line = bytes.TrimRight(line, "\r\n")
	if len(line) > max {
		line = line[:max]
	}
	s.lastLine = append(s.lastLine[:0], line...)
}

func (s *SSEStream) Close() error {
	return s.body.Close()
}
