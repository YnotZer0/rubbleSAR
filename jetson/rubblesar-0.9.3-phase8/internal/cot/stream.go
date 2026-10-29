package cot

import (
	"bufio"
	"bytes"
	"io"
)

// MaxEventBytes bounds one event on the stream (a larger one is skipped).
const MaxEventBytes = 1 << 20

var (
	evOpen  = []byte("<event")
	evClose = []byte("</event>")
)

// Splitter cuts a TCP stream into event documents. XML declarations,
// whitespace and anything else between events are dropped.
type Splitter struct {
	r   *bufio.Reader
	buf []byte
	// Skipped counts oversized or unreadable chunks dropped.
	Skipped int
}

// NewSplitter reads events from r.
func NewSplitter(r io.Reader) *Splitter { return &Splitter{r: bufio.NewReaderSize(r, 64<<10)} }

// Next returns the next complete event document (raw bytes, valid until the
// next call) or the read error.
func (s *Splitter) Next() ([]byte, error) {
	chunk := make([]byte, 32<<10)
	for {
		if doc, ok := s.cut(); ok {
			return doc, nil
		}
		n, err := s.r.Read(chunk)
		if n > 0 {
			s.buf = append(s.buf, chunk[:n]...)
			if len(s.buf) > 2*MaxEventBytes {
				// No end tag in sight: drop all but a tail that may hold a start tag.
				s.Skipped++
				s.buf = append(s.buf[:0], s.buf[len(s.buf)-len(evOpen):]...)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
	}
}

// cut takes one document off the buffer if a complete one is there.
func (s *Splitter) cut() ([]byte, bool) {
	for {
		start := bytes.Index(s.buf, evOpen)
		if start < 0 {
			// Keep a possible partial "<event" at the end.
			if k := len(s.buf) - len(evOpen); k > 0 {
				s.buf = append(s.buf[:0], s.buf[k:]...)
			}
			return nil, false
		}
		// "<event" must be followed by a space, '>' or '/' (not "<eventx").
		if start+len(evOpen) >= len(s.buf) {
			s.buf = append(s.buf[:0], s.buf[start:]...)
			return nil, false
		}
		if c := s.buf[start+len(evOpen)]; c != ' ' && c != '>' && c != '\t' && c != '\n' && c != '\r' && c != '/' {
			s.buf = s.buf[start+len(evOpen):]
			continue
		}
		rest := s.buf[start:]
		// A self-closed <event .../> (no detail at all).
		if gt := bytes.IndexByte(rest, '>'); gt > 0 && rest[gt-1] == '/' {
			if next := bytes.Index(rest[1:], evOpen); next < 0 || next+1 > gt {
				doc := append([]byte(nil), rest[:gt+1]...)
				s.buf = append(s.buf[:0], rest[gt+1:]...)
				return doc, true
			}
		}
		end := bytes.Index(rest, evClose)
		if end < 0 {
			if len(rest) > MaxEventBytes {
				s.Skipped++
				s.buf = append(s.buf[:0], rest[len(evOpen):]...)
				continue
			}
			s.buf = append(s.buf[:0], rest...)
			return nil, false
		}
		doc := append([]byte(nil), rest[:end+len(evClose)]...)
		s.buf = append(s.buf[:0], rest[end+len(evClose):]...)
		return doc, true
	}
}

// ReadEvent reads and parses the next event; documents that are not CoT
// events are counted in Skipped and passed over.
func (s *Splitter) ReadEvent() (*Event, error) {
	for {
		doc, err := s.Next()
		if err != nil {
			return nil, err
		}
		e, perr := Parse(doc)
		if perr != nil {
			s.Skipped++
			continue
		}
		return e, nil
	}
}
