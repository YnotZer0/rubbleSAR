package msg

import (
	"testing"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/sarpb"
)

func TestHeaderRoundTrip(t *testing.T) {
	var s Seq
	if s.Next() != 1 || s.Next() != 2 {
		t.Fatal("Seq must start at 1 and increment")
	}
	tel := &sarpb.Telemetry{Header: NewHeader("inc", "uav/a", 7), UavId: "a", BatteryPct: 88.5}
	b, err := Marshal(tel)
	if err != nil {
		t.Fatal(err)
	}
	var got sarpb.Telemetry
	if err := Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := CheckHeader(got.Header, "inc"); err != nil {
		t.Fatal(err)
	}
	if got.Header.Seq != 7 || got.UavId != "a" || got.BatteryPct != 88.5 {
		t.Fatalf("round trip mismatch: %+v", &got)
	}
	if d := time.Since(SentTime(got.Header)); d < 0 || d > time.Second {
		t.Fatalf("sent time off by %v", d)
	}
}

func TestCheckHeaderErrors(t *testing.T) {
	cases := []struct {
		h    *sarpb.Header
		code diag.Code
	}{
		{nil, "E-MSG-001"},
		{&sarpb.Header{Schema: 2, IncidentId: "inc"}, "E-MSG-002"},
		{&sarpb.Header{Schema: 1, IncidentId: "other"}, "E-MSG-003"},
	}
	for _, c := range cases {
		if got := diag.CodeOf(CheckHeader(c.h, "inc")); got != c.code {
			t.Errorf("CheckHeader(%+v) code %q want %q", c.h, got, c.code)
		}
	}
}

func TestUnmarshalGarbage(t *testing.T) {
	var tel sarpb.Telemetry
	if err := Unmarshal([]byte{0xff, 0xff, 0xff}, &tel); diag.CodeOf(err) != "E-MSG-001" {
		t.Fatalf("want E-MSG-001, got %v", err)
	}
}
