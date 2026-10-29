package keys

import "testing"

func TestValidateID(t *testing.T) {
	good := []string{"a", "scout-1", "antakya-demo", "inv-2", "0abc"}
	bad := []string{"", "-x", "Scout", "a/b", "a*", "has space", "a_b", "abcdefghijklmnopqrstuvwxyz0123456789"}
	for _, id := range good {
		if err := ValidateID("uav", id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range bad {
		if err := ValidateID("uav", id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want error", id)
		}
	}
}

func TestBuilders(t *testing.T) {
	cases := map[string]string{
		Telemetry("inc", "scout-1"):  "sar/inc/uav/scout-1/telemetry",
		TelemetryAll("inc"):          "sar/inc/uav/*/telemetry",
		Control("inc", "inv-1"):      "sar/inc/uav/inv-1/control",
		ControlReply("inc", "inv-1"): "sar/inc/uav/inv-1/control/reply",
		ControlReplyAll("inc"):       "sar/inc/uav/*/control/reply",
		Detection("inc", "inv-1"):    "sar/inc/uav/inv-1/detection",
		DetectionAll("inc"):          "sar/inc/uav/*/detection",
		Ack("inc", "inv-1"):          "sar/inc/uav/inv-1/ack",
		AckAll("inc"):                "sar/inc/uav/*/ack",
		SimTruth("inc", "scout-1"):   "sar/inc/sim/uav/scout-1/truth",
		SimTruthAll("inc"):           "sar/inc/sim/uav/*/truth",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestParseUAV(t *testing.T) {
	k, ok := ParseUAV("sar/inc/uav/scout-1/control/reply")
	if !ok || k.Incident != "inc" || k.UAV != "scout-1" || k.Leaf != "control/reply" {
		t.Fatalf("ParseUAV = %+v %v", k, ok)
	}
	for _, bad := range []string{"", "sar", "sar/inc/uav/x", "foo/inc/uav/x/telemetry", "sar/inc/drone/x/telemetry", "sar//uav/x/telemetry"} {
		if _, ok := ParseUAV(bad); ok {
			t.Errorf("ParseUAV(%q) ok, want not ok", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		expr, key string
		want      bool
	}{
		{"sar/inc/uav/*/telemetry", "sar/inc/uav/scout-1/telemetry", true},
		{"sar/inc/uav/*/telemetry", "sar/inc/uav/scout-1/control", false},
		{"sar/inc/uav/*/telemetry", "sar/inc/uav/a/b/telemetry", false},
		{"sar/**", "sar/inc/uav/a/telemetry", true},
		{"sar/**/reply", "sar/inc/uav/a/control/reply", true},
		{"sar/**/reply", "sar/inc/uav/a/control", false},
		{"sar/inc/**", "sar/inc", true},
		{"sar/inc/uav/scout-1/telemetry", "sar/inc/uav/scout-1/telemetry", true},
		{"sar/inc/uav/scout-1/telemetry", "sar/inc/uav/scout-2/telemetry", false},
		{"sar/*", "sar", false},
	}
	for _, c := range cases {
		if got := Match(c.expr, c.key); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.expr, c.key, got, c.want)
		}
	}
}
