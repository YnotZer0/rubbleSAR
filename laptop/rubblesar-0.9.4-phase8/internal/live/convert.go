package live

import (
	"math"
	"strings"

	"rubblesar/internal/casualty"
	"rubblesar/internal/sarpb"
)

func word(s, prefix string) string { return strings.ToLower(strings.TrimPrefix(s, prefix)) }

func r1(v float64) float64 { return math.Round(v*10) / 10 }
func r2(v float64) float64 { return math.Round(v*100) / 100 }
func r3(v float64) float64 { return math.Round(v*1000) / 1000 }

func nsMS(ns int64) int64 {
	if ns <= 0 {
		return 0
	}
	return ns / 1e6
}

// PartName is a body-part class in words ("hand", "warm spot").
func PartName(p sarpb.PartClass) string {
	if p == sarpb.PartClass_PART_CLASS_WARM_SPOT {
		return "warm spot"
	}
	return word(p.String(), "PART_CLASS_")
}

// FromTelemetry is a drone's picture from its telemetry.
func FromTelemetry(t *sarpb.Telemetry, receivedMS int64) *Drone {
	d := &Drone{ID: t.UavId, Role: word(t.Role.String(), "UAV_ROLE_"), State: word(t.State.String(), "FLIGHT_STATE_"),
		Lat: round6(t.GetPosition().GetLatDeg()), Lon: round6(t.GetPosition().GetLonDeg()), Alt: r1(float64(t.AltAglM)), Yaw: r1(float64(t.YawDeg)),
		Course: r1(float64(t.CourseDeg)), Speed: r1(float64(t.GroundSpeedMps)), Battery: r1(float64(t.BatteryPct)), FlightS: t.FlightTimeLeftS,
		GPSm: r1(float64(t.GpsHaccM)), HomeLat: round6(t.GetHome().GetLatDeg()), HomeLon: round6(t.GetHome().GetLonDeg()), Seen: receivedMS,
		Software: t.Software, Survey: t.SurveySites}
	if p := t.Perception; p != nil {
		d.Step, d.Steps, d.Surveyed, d.SimS = p.MissionStep, p.MissionSteps, p.MissionDone, r1(p.SimTimeS)
		d.Perc = &Perc{Backend: p.Backend, Model: p.Model, HzTarget: r2(float64(p.FrameHzTarget)), Hz: r2(float64(p.FrameHzActual)), Frames: p.Frames,
			SegMS: r1(float64(p.SegmentMs)), ThermMS: r1(float64(p.ThermalMs)), TrackMS: r1(float64(p.TrackMs)), PackMS: r1(float64(p.PackageMs)),
			RenderMS: r1(float64(p.RenderMs)), Tracks: p.Tracks, Reports: p.Reports, Warm: p.WarmSpotReports}
		if !p.Enabled {
			d.Perc = nil
		}
	}
	if l := t.Link; l != nil {
		d.Link = &Link{BudgetBps: l.BudgetBps, UsedBps: l.UsedBps, QueuedMsgs: l.QueuedMsgs, QueuedBytes: l.QueuedBytes, Unacked: l.UnackedMsgs,
			Degrade: l.DegradeLevel, Outage: l.Outage, Probing: l.Probing, Acked: l.AckedMsgs, Resent: l.ResentMsgs, Discarded: l.DiscardedMsgs,
			Dropped: l.DroppedMsgs, SentBytes: l.SentBytes}
	}
	if ts := t.Task; ts != nil && ts.TaskId != "" {
		d.Task = &DroneTask{ID: ts.TaskId, Casualty: ts.CasualtyId, State: word(ts.State.String(), "TASK_STATE_"), Lat: round6(ts.GetTarget().GetLatDeg()),
			Lon: round6(ts.GetTarget().GetLonDeg()), HoverS: ts.HoverS, Elapsed: r1(float64(ts.HoverElapsedS)), Moved: ts.Moved}
	}
	return d
}

// FromCasualty is a casualty's picture.
func FromCasualty(c *sarpb.Casualty) *Casualty {
	out := &Casualty{ID: c.CasualtyId, Triage: casualty.TriageName(c.Triage), Prov: c.Provisional, Status: word(c.Status.String(), "CASUALTY_STATUS_"),
		Lat: round6(c.GetPosition().GetLatDeg()), Lon: round6(c.GetPosition().GetLonDeg()), CEP: r1(float64(c.CepM)), UAVs: c.UavIds,
		First: nsMS(c.FirstSeenUnixNs), Last: nsMS(c.LastSeenUnixNs), Reason: c.Reason, Summary: c.Summary, Conf: r3(float64(c.Confidence)),
		PAlive: r3(float64(c.PAlive)), Close: c.SeenClose, Looks: c.Investigations, NeedsLook: c.NeedsInvestigation, LookWhy: c.InvestigationReason,
		Best: c.BestDetectionId}
	if t := c.Thermal; t != nil {
		out.Thermal = &Thermal{Readable: t.Readable, Warm: t.Warm, SkinC: r1(float64(t.SkinC)), AmbientC: r1(float64(t.AmbientC)), DeltaC: r1(float64(t.DeltaC)),
			Readings: t.Readings, Close: t.CloseReadings, TrendCH: r2(float64(t.TrendCPerH))}
	}
	if m := c.Movement; m != nil {
		out.Moved, out.Watched = m.Moved, r1(float64(m.WatchedS))
	}
	for _, p := range c.Parts {
		out.Parts = append(out.Parts, &Part{UAV: p.UavId, Track: p.TrackId, Det: p.DetectionId, Class: PartName(p.Part), Conf: r3(float64(p.Confidence)), Obs: p.Observations})
	}
	return out
}

// FromTask is an investigation task's picture.
func FromTask(t *sarpb.Task) *Task {
	return &Task{ID: t.TaskId, Casualty: t.CasualtyId, State: word(t.State.String(), "TASK_STATE_"), Tier: t.Tier, Reason: t.Reason, UAV: t.UavId,
		Lat: round6(t.GetTarget().GetLatDeg()), Lon: round6(t.GetTarget().GetLonDeg()), Found: t.GetResult().GetFound(), Moved: t.GetResult().GetMoved(),
		Note: t.Note, Created: nsMS(t.CreatedUnixNs), Finished: nsMS(t.FinishedUnixNs)}
}

// FromTeam is a rescue team's picture.
func FromTeam(t *sarpb.Team) *Team {
	return &Team{ID: t.TeamId, Callsign: t.Callsign, Mode: t.Mode, Source: t.Source, State: word(t.State.String(), "TEAM_STATE_"),
		Lat: round6(t.GetPosition().GetLatDeg()), Lon: round6(t.GetPosition().GetLonDeg()), Course: r1(float64(t.CourseDeg)), Casualty: t.CasualtyId,
		Done: t.AssignmentsDone, Seen: nsMS(t.LastSeenUnixNs)}
}

// FromAssignment is a rescue assignment's picture.
func FromAssignment(a *sarpb.Assignment) *Assignment {
	out := &Assignment{ID: a.AssignmentId, Casualty: a.CasualtyId, Team: a.TeamId, State: word(a.State.String(), "ASSIGNMENT_STATE_"),
		Triage: casualty.TriageName(a.Triage), Prov: a.Provisional, Priority: a.Priority, Created: nsMS(a.CreatedUnixNs), OnScene: nsMS(a.OnSceneUnixNs),
		Done: nsMS(a.DoneUnixNs), Needed: nsMS(a.NeededUnixNs), Extrication: a.Extrication, ExtricationS: math.Round(float64(a.ExtricationS)), Note: a.Note}
	if r := a.Route; r != nil {
		out.LengthM, out.RubbleM, out.ETAs, out.Streets, out.Access = math.Round(float64(r.LengthM)), math.Round(float64(r.RubbleM)), math.Round(float64(r.EtaS)), r.Streets, int(r.AccessIndex)
		for _, p := range r.Points {
			out.Route = append(out.Route, [2]float64{round6(p.LatDeg), round6(p.LonDeg)})
		}
	}
	return out
}

// FromDispatch is rescue-dispatch's state.
func FromDispatch(st *sarpb.DispatchState) Dispatch {
	d := Dispatch{Run: st.Run, Waiting: st.Waiting, Teams: []*Team{}}
	for _, t := range st.Teams {
		d.Teams = append(d.Teams, FromTeam(t))
	}
	for _, a := range st.Assignments {
		d.Assignments = append(d.Assignments, FromAssignment(a))
	}
	if t := st.Tak; t != nil {
		d.TAK = &TAK{Server: t.Server, Connected: t.Connected, Users: t.Users, Sent: t.Sent, Received: t.Received}
	}
	return d
}

var payloadWords = map[sarpb.Payload]string{sarpb.Payload_PAYLOAD_FULL: "full", sarpb.Payload_PAYLOAD_SMALL: "small",
	sarpb.Payload_PAYLOAD_THERMAL_ONLY: "thermal", sarpb.Payload_PAYLOAD_METADATA: "metadata"}

// FromDetection is a detection report's picture (size: its encoded size in bytes).
func FromDetection(d *sarpb.Detection, size int, receivedMS int64) *Det {
	out := &Det{ID: d.DetectionId, UAV: d.UavId, Track: d.TrackId, Rev: d.Revision, Reason: word(d.Reason.String(), "REPORT_REASON_"),
		Part: PartName(d.Part), Conf: r3(float64(d.Confidence)), Fused: r3(float64(d.FusedConfidence)), Lat: round6(d.GetPosition().GetLatDeg()),
		Lon: round6(d.GetPosition().GetLonDeg()), CEP: r1(float64(d.CepM)), Payload: payloadWords[d.Payload], Bytes: size,
		Crop: len(d.GetVisibleCrop().GetData()) > 0, Therm: len(d.GetThermalCrop().GetData()) > 0, At: receivedMS, Capture: nsMS(d.CaptureUnixNs),
		Frame: d.FrameId, RangeM: r1(float64(d.RangeM)), Obs: d.Observations, Summary: d.Summary}
	if t := d.Thermal; t != nil {
		out.Thermal = &Thermal{Readable: t.Readable, Warm: t.Warm, SkinC: r1(float64(t.SkinC)), AmbientC: r1(float64(t.AmbientC)), DeltaC: r1(float64(t.DeltaC))}
	}
	for _, v := range d.Outline {
		out.Outline = append(out.Outline, r3(float64(v)))
	}
	return out
}
