// Package msg holds helpers shared by every producer and consumer of
// RubbleSAR protobuf messages: headers, encoding and validation.
package msg

import (
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/diag"
	"rubblesar/internal/sarpb"
)

// SchemaVersion is the current Header.schema major version.
const SchemaVersion = 1

// Seq is a per-topic sequence counter starting at 1.
type Seq struct{ n atomic.Uint64 }

// Next returns the next sequence number.
func (s *Seq) Next() uint64 { return s.n.Add(1) }

// NewHeader builds a header stamped with the current UTC time.
func NewHeader(incident, source string, seq uint64) *sarpb.Header {
	return &sarpb.Header{
		Schema:     SchemaVersion,
		IncidentId: incident,
		SourceId:   source,
		Seq:        seq,
		SentUnixNs: time.Now().UnixNano(),
	}
}

// CheckHeader validates schema version and incident id.
func CheckHeader(h *sarpb.Header, incident string) error {
	if h == nil {
		return diag.New("E-MSG-001", "message has no header")
	}
	if h.GetSchema() != SchemaVersion {
		return diag.New("E-MSG-002", "schema %d from %s, this build understands %d", h.GetSchema(), h.GetSourceId(), SchemaVersion)
	}
	if h.GetIncidentId() != incident {
		return diag.New("E-MSG-003", "message for incident %q from %s, expected %q", h.GetIncidentId(), h.GetSourceId(), incident)
	}
	return nil
}

// Marshal encodes a message.
func Marshal(m proto.Message) ([]byte, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, diag.Wrap("E-MSG-001", err, "encoding %T", m)
	}
	return b, nil
}

// Unmarshal decodes a message, mapping failures to E-MSG-001.
func Unmarshal(b []byte, m proto.Message) error {
	if err := proto.Unmarshal(b, m); err != nil {
		return diag.Wrap("E-MSG-001", err, "decoding %T (%d bytes)", m, len(b))
	}
	return nil
}

// SentTime returns the header's send time.
func SentTime(h *sarpb.Header) time.Time { return time.Unix(0, h.GetSentUnixNs()) }
