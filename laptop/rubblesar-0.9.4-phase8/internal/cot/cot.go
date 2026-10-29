// Package cot builds, reads and sends Cursor-on-Target (CoT) events, the XML
// messages TAK clients (ATAK, WinTAK) and TAK servers (taky) exchange.
//
// Phase 5 uses plain TCP (plan decision D10, taky's port 8087): one event
// after another on the stream, each a complete <event>...</event> document.
//
// Event types used:
//
//	b-m-p-s-m   spot marker (casualties, coloured by triage)
//	a-f-A-M-H-Q friendly rotary-wing drone
//	a-f-G-U-C   friendly ground unit (rescue teams, SAR-OPS)
//	u-d-f       free-form line (team routes, investigation lines)
//	b-t-f       GeoChat message
//	t-x-d-d     delete an object
//	t-x-c-t     ping
package cot

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"rubblesar/internal/diag"
)

// TimeFormat is the CoT time layout (UTC, milliseconds).
const TimeFormat = "2006-01-02T15:04:05.000Z"

// NoValue is the CoT "unknown" for hae, ce and le.
const NoValue = 9999999.0

// AllChat is ATAK's broadcast chat room.
const AllChat = "All Chat Rooms"

// ARGB colours (signed 32-bit, as ATAK writes them).
const (
	Red    int32 = -65536    // 0xFFFF0000
	Yellow int32 = -256      // 0xFFFFFF00
	Green  int32 = -16711936 // 0xFF00FF00
	Black  int32 = -16777216 // 0xFF000000
	Grey   int32 = -7829368  // 0xFF888888
	White  int32 = -1        // 0xFFFFFFFF
	Cyan   int32 = -16711681 // 0xFF00FFFF
	Blue   int32 = -16776961 // 0xFF0000FF
	Orange int32 = -23296    // 0xFFFFA500
	Purple int32 = -8388480  // 0xFF800080
)

// Event is one CoT event. Unknown detail elements are ignored when reading.
type Event struct {
	XMLName xml.Name `xml:"event"`
	Version string   `xml:"version,attr"`
	UID     string   `xml:"uid,attr"`
	Type    string   `xml:"type,attr"`
	How     string   `xml:"how,attr"`
	Time    string   `xml:"time,attr"`
	Start   string   `xml:"start,attr"`
	Stale   string   `xml:"stale,attr"`
	Point   Point    `xml:"point"`
	Detail  Detail   `xml:"detail"`
}

// Point is the event position: WGS84 degrees, height above the ellipsoid
// (hae) and circular/linear error (ce, le) in metres.
type Point struct {
	Lat Num `xml:"lat,attr"`
	Lon Num `xml:"lon,attr"`
	Hae Num `xml:"hae,attr"`
	CE  Num `xml:"ce,attr"`
	LE  Num `xml:"le,attr"`
}

// Num is a number attribute written in plain decimal (9999999.0, not 1e+07).
type Num float64

// MarshalXMLAttr writes the number.
func (n Num) MarshalXMLAttr(name xml.Name) (xml.Attr, error) {
	s := strconv.FormatFloat(float64(n), 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return xml.Attr{Name: name, Value: s}, nil
}

// UnmarshalXMLAttr reads the number (an unreadable one reads as 0).
func (n *Num) UnmarshalXMLAttr(a xml.Attr) error {
	v, err := strconv.ParseFloat(strings.TrimSpace(a.Value), 64)
	if err != nil {
		v = 0
	}
	*n = Num(v)
	return nil
}

// Detail holds the detail elements RubbleSAR writes or reads.
type Detail struct {
	Contact      *Contact   `xml:"contact,omitempty"`
	Group        *Group     `xml:"__group,omitempty"`
	Takv         *Takv      `xml:"takv,omitempty"`
	Track        *Track     `xml:"track,omitempty"`
	Status       *Status    `xml:"status,omitempty"`
	Chat         *Chat      `xml:"__chat,omitempty"`
	Links        []Link     `xml:"link,omitempty"`
	Color        *Color     `xml:"color,omitempty"`
	UserIcon     *UserIcon  `xml:"usericon,omitempty"`
	StrokeColor  *Value     `xml:"strokeColor,omitempty"`
	StrokeWeight *Value     `xml:"strokeWeight,omitempty"`
	StrokeStyle  *Value     `xml:"strokeStyle,omitempty"`
	FillColor    *Value     `xml:"fillColor,omitempty"`
	LabelsOn     *Value     `xml:"labels_on,omitempty"`
	Remarks      *Remarks   `xml:"remarks,omitempty"`
	Marti        *Marti     `xml:"marti,omitempty"`
	ForceDelete  *struct{}  `xml:"__forcedelete,omitempty"`
	Archive      *struct{}  `xml:"archive,omitempty"`
	Precision    *Precision `xml:"precisionlocation,omitempty"`
}

// Contact names the object (its callsign, shown as its label).
type Contact struct {
	Callsign string `xml:"callsign,attr"`
	Endpoint string `xml:"endpoint,attr,omitempty"`
}

// Group is the team colour and role of a user.
type Group struct {
	Name string `xml:"name,attr"`
	Role string `xml:"role,attr"`
}

// Takv identifies the client software.
type Takv struct {
	Device   string `xml:"device,attr,omitempty"`
	Platform string `xml:"platform,attr,omitempty"`
	OS       string `xml:"os,attr,omitempty"`
	Version  string `xml:"version,attr,omitempty"`
}

// Track is course (degrees true) and speed (m/s).
type Track struct {
	Course Num `xml:"course,attr"`
	Speed  Num `xml:"speed,attr"`
}

// Status carries the battery level of a device.
type Status struct {
	Battery string `xml:"battery,attr,omitempty"`
}

// Chat is the GeoChat header.
type Chat struct {
	Parent         string   `xml:"parent,attr,omitempty"`
	GroupOwner     string   `xml:"groupOwner,attr,omitempty"`
	MessageID      string   `xml:"messageId,attr,omitempty"`
	Chatroom       string   `xml:"chatroom,attr"`
	ID             string   `xml:"id,attr"`
	SenderCallsign string   `xml:"senderCallsign,attr"`
	ChatGrp        *ChatGrp `xml:"chatgrp,omitempty"`
}

// ChatGrp lists the chat's members.
type ChatGrp struct {
	UID0 string `xml:"uid0,attr"`
	UID1 string `xml:"uid1,attr"`
	ID   string `xml:"id,attr"`
}

// Link is a relation to another object, or one vertex of a shape.
type Link struct {
	UID      string `xml:"uid,attr,omitempty"`
	Type     string `xml:"type,attr,omitempty"`
	Relation string `xml:"relation,attr,omitempty"`
	Point    string `xml:"point,attr,omitempty"`
}

// Color is a marker colour (ARGB).
type Color struct {
	ARGB int32 `xml:"argb,attr"`
}

// UserIcon selects the marker icon.
type UserIcon struct {
	IconSetPath string `xml:"iconsetpath,attr"`
}

// Value is an element with one value attribute.
type Value struct {
	Value string `xml:"value,attr"`
}

// Remarks is free text.
type Remarks struct {
	Source string `xml:"source,attr,omitempty"`
	To     string `xml:"to,attr,omitempty"`
	Time   string `xml:"time,attr,omitempty"`
	Text   string `xml:",chardata"`
}

// Marti addresses an event to particular users (by callsign).
type Marti struct {
	Dest []Dest `xml:"dest"`
}

// Dest is one addressee.
type Dest struct {
	Callsign string `xml:"callsign,attr"`
}

// Precision tells where the position came from.
type Precision struct {
	GeoPointSrc string `xml:"geopointsrc,attr,omitempty"`
	AltSrc      string `xml:"altsrc,attr,omitempty"`
}

// FormatTime writes a CoT time.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a CoT time (with or without fractional seconds).
func ParseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// New starts an event of type typ for uid, valid from now until now+stale.
func New(uid, typ, how string, now time.Time, stale time.Duration) *Event {
	t := FormatTime(now)
	return &Event{Version: "2.0", UID: uid, Type: typ, How: how, Time: t, Start: t, Stale: FormatTime(now.Add(stale)),
		Point: Point{Hae: NoValue, CE: NoValue, LE: NoValue}}
}

// At sets the position (hae and errors in metres; NaN = unknown).
func (e *Event) At(lat, lon, hae, ce, le float64) *Event {
	e.Point = Point{Lat: Num(round(lat, 7)), Lon: Num(round(lon, 7)), Hae: Num(orNo(hae)), CE: Num(orNo(ce)), LE: Num(orNo(le))}
	return e
}

func orNo(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return NoValue
	}
	return round(v, 1)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// Marshal writes the event as one XML document.
func (e *Event) Marshal() ([]byte, error) {
	// No <?xml?> declaration: a stream of bare <event> documents is what every
	// TAK server accepts.
	var b bytes.Buffer
	enc := xml.NewEncoder(&b)
	if err := enc.Encode(e); err != nil {
		return nil, diag.Wrap("E-TAK-004", err, "encoding CoT event %s", e.UID)
	}
	return b.Bytes(), nil
}

// Parse reads one event document.
func Parse(b []byte) (*Event, error) {
	var e Event
	if err := xml.Unmarshal(b, &e); err != nil {
		return nil, diag.Wrap("E-TAK-004", err, "not a CoT event")
	}
	if e.UID == "" || e.Type == "" {
		return nil, diag.New("E-TAK-004", "CoT event without uid or type")
	}
	return &e, nil
}

// Callsign returns the contact callsign ("" if none).
func (e *Event) Callsign() string {
	if e.Detail.Contact == nil {
		return ""
	}
	return e.Detail.Contact.Callsign
}

// StaleTime parses the stale attribute (zero if unreadable).
func (e *Event) StaleTime() time.Time {
	t, _ := ParseTime(e.Stale)
	return t
}

// IsPLI reports a position report of a ground user (a TAK client's own position).
func (e *Event) IsPLI() bool {
	return strings.HasPrefix(e.Type, "a-f-G") && e.Detail.Contact != nil
}

// IsChat reports a GeoChat message.
func (e *Event) IsChat() bool { return e.Type == "b-t-f" && e.Detail.Chat != nil }

// IsDelete reports a delete; DeleteUID names the object deleted.
func (e *Event) IsDelete() bool { return e.Type == "t-x-d-d" }

// DeleteUID is the object a t-x-d-d deletes.
func (e *Event) DeleteUID() string {
	for _, l := range e.Detail.Links {
		if l.UID != "" {
			return l.UID
		}
	}
	return ""
}

// ChatText returns the message text of a GeoChat.
func (e *Event) ChatText() string {
	if e.Detail.Remarks == nil {
		return ""
	}
	return strings.TrimSpace(e.Detail.Remarks.Text)
}

// ChatSender returns the sender's uid of a GeoChat (from the link or chatgrp).
func (e *Event) ChatSender() string {
	for _, l := range e.Detail.Links {
		if l.Relation == "p-p" && l.UID != "" {
			return l.UID
		}
	}
	if c := e.Detail.Chat; c != nil && c.ChatGrp != nil {
		return c.ChatGrp.UID0
	}
	return ""
}

// Dests returns the marti addressees.
func (e *Event) Dests() []string {
	if e.Detail.Marti == nil {
		return nil
	}
	var out []string
	for _, d := range e.Detail.Marti.Dest {
		if d.Callsign != "" {
			out = append(out, d.Callsign)
		}
	}
	return out
}

// LinePoints returns the vertices of a u-d-f line as lat, lon pairs.
func (e *Event) LinePoints() [][2]float64 {
	var out [][2]float64
	for _, l := range e.Detail.Links {
		if l.Point == "" {
			continue
		}
		f := strings.Split(l.Point, ",")
		if len(f) < 2 {
			continue
		}
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		if err1 == nil && err2 == nil {
			out = append(out, [2]float64{lat, lon})
		}
	}
	return out
}

// MarkerColor returns the marker colour (ok false without one).
func (e *Event) MarkerColor() (int32, bool) {
	if e.Detail.Color != nil {
		return e.Detail.Color.ARGB, true
	}
	if e.Detail.UserIcon != nil {
		p := e.Detail.UserIcon.IconSetPath
		if i := strings.LastIndex(p, "/"); i >= 0 {
			if v, err := strconv.ParseInt(p[i+1:], 10, 32); err == nil {
				return int32(v), true
			}
		}
	}
	return 0, false
}

// --- Builders -------------------------------------------------------------------

// Marker is a spot marker (b-m-p-s-m) with a colour, label and remarks.
func Marker(uid, label string, colour int32, lat, lon, hae, ce float64, remarks string, now time.Time, stale time.Duration) *Event {
	e := New(uid, "b-m-p-s-m", "h-g-i-g-o", now, stale).At(lat, lon, hae, ce, math.NaN())
	e.Detail.Contact = &Contact{Callsign: label}
	e.Detail.Color = &Color{ARGB: colour}
	e.Detail.UserIcon = &UserIcon{IconSetPath: fmt.Sprintf("COT_MAPPING_SPOTMAP/b-m-p-s-m/%d", colour)}
	e.Detail.Precision = &Precision{GeoPointSrc: "CALC", AltSrc: "CALC"}
	if remarks != "" {
		e.Detail.Remarks = &Remarks{Text: remarks}
	}
	return e
}

// Drone is a friendly rotary-wing drone (a-f-A-M-H-Q) moving at course/speed.
func Drone(uid, callsign string, lat, lon, hae, course, speed float64, battery int, remarks string, now time.Time, stale time.Duration) *Event {
	e := New(uid, "a-f-A-M-H-Q", "m-g", now, stale).At(lat, lon, hae, 5, 5)
	e.Detail.Contact = &Contact{Callsign: callsign}
	e.Detail.Track = &Track{Course: Num(round(course, 1)), Speed: Num(round(speed, 1))}
	if battery >= 0 {
		e.Detail.Status = &Status{Battery: strconv.Itoa(battery)}
	}
	if remarks != "" {
		e.Detail.Remarks = &Remarks{Text: remarks}
	}
	return e
}

// PLI is a ground user's position (a-f-G-U-C), e.g. a rescue team.
func PLI(uid, callsign, group, role string, lat, lon, course, speed float64, remarks string, now time.Time, stale time.Duration) *Event {
	e := New(uid, "a-f-G-U-C", "m-g", now, stale).At(lat, lon, math.NaN(), 10, math.NaN())
	e.Detail.Contact = &Contact{Callsign: callsign, Endpoint: "*:-1:stcp"}
	e.Detail.Group = &Group{Name: group, Role: role}
	e.Detail.Takv = &Takv{Device: "ZBook", Platform: "RubbleSAR", OS: "linux", Version: "phase5"}
	e.Detail.Track = &Track{Course: Num(round(course, 1)), Speed: Num(round(speed, 1))}
	if remarks != "" {
		e.Detail.Remarks = &Remarks{Text: remarks}
	}
	return e
}

// Line is a free-form polyline (u-d-f) through pts (lat, lon), with a label.
func Line(uid, label string, colour int32, weight float64, pts [][2]float64, remarks string, now time.Time, stale time.Duration) *Event {
	e := New(uid, "u-d-f", "h-e", now, stale)
	if len(pts) > 0 {
		e.At(pts[0][0], pts[0][1], math.NaN(), math.NaN(), math.NaN())
	}
	e.Detail.Contact = &Contact{Callsign: label}
	for _, p := range pts {
		e.Detail.Links = append(e.Detail.Links, Link{Point: fmt.Sprintf("%.7f,%.7f", p[0], p[1])})
	}
	e.Detail.StrokeColor = &Value{Value: strconv.Itoa(int(colour))}
	e.Detail.StrokeWeight = &Value{Value: strconv.FormatFloat(weight, 'f', 1, 64)}
	e.Detail.LabelsOn = &Value{Value: "true"}
	if remarks != "" {
		e.Detail.Remarks = &Remarks{Text: remarks}
	}
	return e
}

// Polygon is a closed, filled shape (u-d-f whose last point is its first),
// e.g. a rubble area.
func Polygon(uid, label string, stroke, fill int32, weight float64, pts [][2]float64, remarks string, now time.Time, stale time.Duration) *Event {
	ring := append([][2]float64(nil), pts...)
	if len(ring) > 0 && ring[0] != ring[len(ring)-1] {
		ring = append(ring, ring[0])
	}
	e := Line(uid, label, stroke, weight, ring, remarks, now, stale)
	if len(pts) > 0 {
		var la, lo float64
		for _, p := range pts {
			la, lo = la+p[0], lo+p[1]
		}
		e.At(la/float64(len(pts)), lo/float64(len(pts)), math.NaN(), math.NaN(), math.NaN())
	}
	e.Detail.FillColor = &Value{Value: strconv.Itoa(int(fill))}
	return e
}

// ARGB packs a colour as TAK wants it (a signed 32-bit ARGB integer).
func ARGB(a, r, g, b uint8) int32 {
	return int32(uint32(a)<<24 | uint32(r)<<16 | uint32(g)<<8 | uint32(b))
}

// Delete removes the object uid from every map.
func Delete(uid string, now time.Time) *Event {
	e := New(uid+".delete."+strconv.FormatInt(now.UnixNano(), 36), "t-x-d-d", "h-g-i-g-o", now, 20*time.Second)
	e.Detail.Links = []Link{{UID: uid, Relation: "none", Type: "none"}}
	e.Detail.ForceDelete = &struct{}{}
	return e
}

// Ping is a keep-alive (t-x-c-t).
func Ping(uid string, now time.Time) *Event {
	return New(uid+"-ping", "t-x-c-t", "h-g-i-g-o", now, 20*time.Second)
}

// ChatTo is who a GeoChat goes to: the All Chat Rooms broadcast (UID "")
// or one user (their uid and callsign).
type ChatTo struct {
	UID      string
	Callsign string
}

// GeoChat is a chat message from sender (uid, callsign) at lat, lon.
func GeoChat(senderUID, senderCallsign string, to ChatTo, text string, lat, lon float64, now time.Time) *Event {
	id := strconv.FormatInt(now.UnixNano(), 36)
	room, roomID := AllChat, AllChat
	if to.UID != "" {
		room, roomID = to.Callsign, to.UID
	}
	e := New("GeoChat."+senderUID+"."+roomID+"."+id, "b-t-f", "h-g-i-g-o", now, 24*time.Hour).At(lat, lon, math.NaN(), math.NaN(), math.NaN())
	e.Detail.Chat = &Chat{Parent: "RootContactGroup", GroupOwner: "false", MessageID: id, Chatroom: room, ID: roomID, SenderCallsign: senderCallsign,
		ChatGrp: &ChatGrp{UID0: senderUID, UID1: roomID, ID: roomID}}
	e.Detail.Links = []Link{{UID: senderUID, Type: "a-f-G-U-C", Relation: "p-p"}}
	e.Detail.Remarks = &Remarks{Source: "BAO.F.ATAK." + senderUID, To: roomID, Time: FormatTime(now), Text: text}
	if to.UID != "" {
		e.Detail.Marti = &Marti{Dest: []Dest{{Callsign: to.Callsign}}}
	}
	return e
}
