// Package keys builds and parses RubbleSAR Zenoh key expressions.
//
// Layout (see docs/ENVIRONMENT.md):
//
//	sar/{incident}/uav/{uav}/telemetry
//	sar/{incident}/uav/{uav}/control
//	sar/{incident}/uav/{uav}/control/reply
//	sar/{incident}/uav/{uav}/detection
//	sar/{incident}/uav/{uav}/ack
//	sar/{incident}/sim/uav/{uav}/truth      (simulation side channel)
//	sar/{incident}/casualty/{casualty}      (phase 4, casualty-analyst)
//	sar/{incident}/casualties               (phase 4, the full list)
//	sar/{incident}/tasks                    (phase 4, mission-planner)
//	sar/{incident}/dispatch                 (phase 5, rescue-dispatch)
package keys

import (
	"regexp"
	"strings"

	"rubblesar/internal/diag"
)

// Root is the first chunk of every key.
const Root = "sar"

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidateID checks an incident or drone id.
func ValidateID(kind, id string) error {
	if !idRE.MatchString(id) {
		return diag.New("E-BUS-003", "%s id %q must match [a-z0-9][a-z0-9-]{0,31}", kind, id)
	}
	return nil
}

func uav(inc, id string) string { return Root + "/" + inc + "/uav/" + id }

// Telemetry is the key a drone publishes telemetry on.
func Telemetry(inc, id string) string { return uav(inc, id) + "/telemetry" }

// TelemetryAll matches telemetry from every drone in an incident.
func TelemetryAll(inc string) string { return uav(inc, "*") + "/telemetry" }

// Control is the key the ZBook sends commands to a drone on.
func Control(inc, id string) string { return uav(inc, id) + "/control" }

// ControlReply is the key a drone answers commands on.
func ControlReply(inc, id string) string { return uav(inc, id) + "/control/reply" }

// ControlReplyAll matches replies from every drone in an incident.
func ControlReplyAll(inc string) string { return uav(inc, "*") + "/control/reply" }

// Detection is the key a drone publishes detection reports on.
func Detection(inc, id string) string { return uav(inc, id) + "/detection" }

// DetectionAll matches detections from every drone in an incident.
func DetectionAll(inc string) string { return uav(inc, "*") + "/detection" }

// Ack is the key the ZBook acknowledges a drone's detections on.
func Ack(inc, id string) string { return uav(inc, id) + "/ack" }

// AckAll matches acknowledgements to every drone in an incident.
func AckAll(inc string) string { return uav(inc, "*") + "/ack" }

// SimTruth is the simulation-only ground-truth channel of a drone's frames.
func SimTruth(inc, id string) string { return Root + "/" + inc + "/sim/uav/" + id + "/truth" }

// SimTruthAll matches the ground-truth channel of every drone.
func SimTruthAll(inc string) string { return SimTruth(inc, "*") }

// Casualty is the key one casualty record is published on.
func Casualty(inc, id string) string { return Root + "/" + inc + "/casualty/" + id }

// CasualtyAll matches every casualty record of an incident.
func CasualtyAll(inc string) string { return Casualty(inc, "*") }

// Casualties is the key of the full casualty list (published periodically).
func Casualties(inc string) string { return Root + "/" + inc + "/casualties" }

// Tasks is the key of mission-planner's task list.
func Tasks(inc string) string { return Root + "/" + inc + "/tasks" }

// Dispatch is the key of rescue-dispatch's teams and assignments.
func Dispatch(inc string) string { return Root + "/" + inc + "/dispatch" }

// UAVKey is a parsed drone key.
type UAVKey struct {
	Incident string
	UAV      string
	Leaf     string // e.g. "telemetry", "control", "control/reply"
}

// ParseUAV splits a concrete drone key. ok is false for anything else.
func ParseUAV(key string) (UAVKey, bool) {
	parts := strings.SplitN(key, "/", 5)
	if len(parts) < 5 || parts[0] != Root || parts[2] != "uav" {
		return UAVKey{}, false
	}
	if parts[1] == "" || parts[3] == "" || parts[4] == "" {
		return UAVKey{}, false
	}
	return UAVKey{Incident: parts[1], UAV: parts[3], Leaf: parts[4]}, true
}

// Match reports whether a concrete key matches a key expression using Zenoh
// semantics for the subset RubbleSAR uses: '*' matches exactly one chunk and
// '**' matches zero or more chunks. It is used by the in-memory bus.
func Match(expr, key string) bool {
	return matchChunks(strings.Split(expr, "/"), strings.Split(key, "/"))
}

func matchChunks(e, k []string) bool {
	for len(e) > 0 {
		switch e[0] {
		case "**":
			if len(e) == 1 {
				return true
			}
			for i := 0; i <= len(k); i++ {
				if matchChunks(e[1:], k[i:]) {
					return true
				}
			}
			return false
		case "*":
			if len(k) == 0 || k[0] == "" {
				return false
			}
		default:
			if len(k) == 0 || e[0] != k[0] {
				return false
			}
		}
		e, k = e[1:], k[1:]
	}
	return len(k) == 0
}
