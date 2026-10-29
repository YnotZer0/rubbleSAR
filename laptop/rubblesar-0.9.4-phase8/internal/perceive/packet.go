package perceive

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/sarpb"
)

// Crop sizes and JPEG qualities per payload level.
const (
	CropFull     = 128
	CropSmall    = 64
	jpegFull     = 75
	jpegSmall    = 70
	maxOutline   = 32
	minCropSrcPx = 96
)

// CropRegion returns the frame region a crop of a part shows: its box grown by
// a quarter on every side (at least 8 px), at least minCropSrcPx square,
// clamped to the frame.
func CropRegion(box [4]float64, w, h int) raster.Rect {
	bw, bh := box[2]-box[0], box[3]-box[1]
	gx, gy := math.Max(0.25*bw, 8), math.Max(0.25*bh, 8)
	x0, y0, x1, y1 := box[0]-gx, box[1]-gy, box[2]+gx, box[3]+gy
	if x1-x0 < minCropSrcPx {
		c := (x0 + x1) / 2
		x0, x1 = c-minCropSrcPx/2, c+minCropSrcPx/2
	}
	if y1-y0 < minCropSrcPx {
		c := (y0 + y1) / 2
		y0, y1 = c-minCropSrcPx/2, c+minCropSrcPx/2
	}
	r := raster.Rect{X0: int(math.Floor(x0)), Y0: int(math.Floor(y0)), X1: int(math.Ceil(x1)), Y1: int(math.Ceil(y1))}
	return r.Intersect(raster.Rect{X1: w, Y1: h})
}

// shrink area-averages an image so its longer side is at most maxSide.
func shrink(src *raster.RGB, maxSide int) *raster.RGB {
	s := math.Max(float64(src.W), float64(src.H)) / float64(maxSide)
	if s <= 1 {
		return src
	}
	w, h := max(1, int(math.Round(float64(src.W)/s))), max(1, int(math.Round(float64(src.H)/s)))
	out := raster.NewRGB(w, h)
	for y := 0; y < h; y++ {
		sy0, sy1 := int(float64(y)*s), max(int(float64(y)*s)+1, int(float64(y+1)*s))
		for x := 0; x < w; x++ {
			sx0, sx1 := int(float64(x)*s), max(int(float64(x)*s)+1, int(float64(x+1)*s))
			var acc [3]int
			n := 0
			for yy := sy0; yy < min(sy1, src.H); yy++ {
				for xx := sx0; xx < min(sx1, src.W); xx++ {
					c := src.At(xx, yy)
					acc[0] += int(c[0])
					acc[1] += int(c[1])
					acc[2] += int(c[2])
					n++
				}
			}
			if n > 0 {
				out.Set(x, y, [3]uint8{uint8(acc[0] / n), uint8(acc[1] / n), uint8(acc[2] / n)})
			}
		}
	}
	return out
}

func encodeJPEG(img *raster.RGB, q int) []byte {
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img.ToNRGBA(), &jpeg.Options{Quality: q})
	return b.Bytes()
}

// thermalPNG encodes a thermal region as 16-bit grey, value = kelvin x 10.
func thermalPNG(th *raster.U16, r raster.Rect) []byte {
	img := image.NewGray16(image.Rect(0, 0, r.W(), r.H()))
	for y := 0; y < r.H(); y++ {
		for x := 0; x < r.W(); x++ {
			img.SetGray16(x, y, color.Gray16{Y: th.Pix[(r.Y0+y)*th.W+r.X0+x] / 10})
		}
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	_ = enc.Encode(&b, img)
	return b.Bytes()
}

// Outline traces a mask, simplifies it to at most maxOutline points and
// expresses it relative to a crop region (0..1).
func Outline(m *masks.Bitmap, r raster.Rect) []float32 {
	if m == nil || m.W == 0 || m.H == 0 {
		return nil
	}
	poly := masks.Trace(*m)
	if len(poly) < 3 {
		return nil
	}
	pts := make([][2]float64, len(poly))
	for i, p := range poly {
		pts[i] = [2]float64{float64(p[0]), float64(p[1])}
	}
	eps := 0.5
	s := masks.Simplify(pts, eps)
	for len(s) > maxOutline {
		eps *= 1.5
		s = masks.Simplify(pts, eps)
	}
	out := make([]float32, 0, 2*len(s))
	for _, p := range s {
		out = append(out, float32(math.Round((p[0]-float64(r.X0))/float64(r.W())*1000)/1000),
			float32(math.Round((p[1]-float64(r.Y0))/float64(r.H())*1000)/1000))
	}
	return out
}

// MakeEvidence builds the crops and outline of a sighting from its frame. box
// and mask are in colour-frame pixels; th may be nil.
func MakeEvidence(rgb *raster.RGB, th *raster.U16, tm ThermalMap, box [4]float64, mask *masks.Bitmap) *Evidence {
	r := CropRegion(box, rgb.W, rgb.H)
	if r.Empty() {
		return nil
	}
	ev := &Evidence{VisBox: [4]float32{float32(r.X0), float32(r.Y0), float32(r.X1), float32(r.Y1)}}
	vis := shrink(rgb.Crop(r), CropFull)
	ev.VisibleJPEG, ev.VisW, ev.VisH = encodeJPEG(vis, jpegFull), vis.W, vis.H
	ev.Outline = Outline(mask, r)
	if th != nil {
		u0, v0 := tm.ToThermal(float64(r.X0), float64(r.Y0))
		u1, v1 := tm.ToThermal(float64(r.X1), float64(r.Y1))
		tr := raster.Rect{X0: int(math.Floor(u0)), Y0: int(math.Floor(v0)), X1: int(math.Ceil(u1)), Y1: int(math.Ceil(v1))}
		if tr.W() < 8 {
			c := (tr.X0 + tr.X1) / 2
			tr.X0, tr.X1 = c-4, c+4
		}
		if tr.H() < 8 {
			c := (tr.Y0 + tr.Y1) / 2
			tr.Y0, tr.Y1 = c-4, c+4
		}
		tr = tr.Intersect(raster.Rect{X1: th.W, Y1: th.H})
		if !tr.Empty() {
			ev.ThermalPNG, ev.ThW, ev.ThH = thermalPNG(th, tr), tr.W(), tr.H()
			ev.ThBox = [4]float32{float32(tr.X0), float32(tr.Y0), float32(tr.X1), float32(tr.Y1)}
		}
	}
	return ev
}

// PartNames are the classes as written in summaries; index WarmClass is a warm spot.
var PartNames = []string{"hand", "arm", "leg", "head", "torso", "warm spot"}

// Summary is the one-line description carried with every packet.
func Summary(t *Track, cep float64, uav string) string {
	var b strings.Builder
	c := t.Class()
	name := PartNames[c]
	if c == WarmClass {
		fmt.Fprintf(&b, "Warm spot, no body part seen in colour")
	} else {
		fmt.Fprintf(&b, "%s%s exposed", strings.ToUpper(name[:1]), name[1:])
		if t.AreaM2 > 0 {
			fmt.Fprintf(&b, ", about %.2f m2 visible", t.AreaM2)
		}
	}
	th := t.Thermal
	switch {
	case th.Readable:
		fmt.Fprintf(&b, ", %.1f C against %.1f C around (%+.1f C)", th.SkinC, th.AmbientC, th.DeltaC)
	case th.Reason != "":
		fmt.Fprintf(&b, ", temperature not readable: %s", th.Reason)
	}
	fmt.Fprintf(&b, ", seen in %d frame", t.Hits)
	if t.Hits != 1 {
		b.WriteString("s")
	}
	if t.Best != nil {
		fmt.Fprintf(&b, " by %s from %.0f m", uav, t.Best.Pose.AltAGL)
	}
	fmt.Fprintf(&b, ", +-%.1f m", cep)
	return b.String()
}

var partEnum = []sarpb.PartClass{sarpb.PartClass_PART_CLASS_HAND, sarpb.PartClass_PART_CLASS_ARM, sarpb.PartClass_PART_CLASS_LEG,
	sarpb.PartClass_PART_CLASS_HEAD, sarpb.PartClass_PART_CLASS_TORSO, sarpb.PartClass_PART_CLASS_WARM_SPOT}

// PartEnum converts a class index (0..4, WarmClass) to the wire enum.
func PartEnum(c int) sarpb.PartClass {
	if c < 0 || c >= len(partEnum) {
		return sarpb.PartClass_PART_CLASS_UNSPECIFIED
	}
	return partEnum[c]
}

// PartIndex converts the wire enum back to a class index (-1 if unknown).
func PartIndex(p sarpb.PartClass) int {
	for i, e := range partEnum {
		if e == p {
			return i
		}
	}
	return -1
}

// Position converts local metres to WGS84 (provided by the caller's scenario).
type Position func(e, n float64) (lat, lon, altMSL float64)

// Packet builds the detection message for a report (without its header).
func Packet(rep Report, tr *Tracker, uav string, pos Position) *sarpb.Detection {
	t := rep.Track
	cep := tr.CEP(t)
	lat, lon, alt := pos(t.E, t.N)
	d := &sarpb.Detection{
		UavId:           uav,
		TrackId:         t.ID,
		Revision:        uint32(t.Revision + 1),
		DetectionId:     fmt.Sprintf("%s.%d", t.ID, t.Revision+1),
		Reason:          sarpb.ReportReason(rep.Reason),
		Part:            PartEnum(t.Class()),
		Confidence:      float32(math.Round(float64(t.BestConf)*1000) / 1000),
		FusedConfidence: float32(t.Fused()),
		Position:        &sarpb.GeoPoint{LatDeg: lat, LonDeg: lon, AltMslM: alt},
		CepM:            float32(math.Round(cep*100) / 100),
		AreaM2:          float32(math.Round(t.AreaM2*10000) / 10000),
		Observations:    uint32(t.Hits),
		FirstSeenUnixNs: t.FirstSeen.UnixNano(),
		LastSeenUnixNs:  t.LastSeen.UnixNano(),
		Thermal:         &sarpb.ThermalReading{Readable: t.Thermal.Readable, NotReadableReason: t.Thermal.Reason, SkinC: r1(t.Thermal.SkinC), AmbientC: r1(t.Thermal.AmbientC), DeltaC: r1(t.Thermal.DeltaC), Pixels: uint32(t.Thermal.Pixels), Warm: t.Thermal.Warm},
		Summary:         Summary(t, cep, uav),
		Payload:         sarpb.Payload_PAYLOAD_FULL,
	}
	for c := 0; c < 5; c++ {
		d.ClassVotes = append(d.ClassVotes, float32(math.Round(t.Votes[c]*1000)/1000))
	}
	d.MinRangeM = float32(math.Round(t.MinRange*10) / 10)
	if m := t.Motion; m != nil && m.Frames > 0 {
		d.Movement = &sarpb.Movement{HoverFrames: uint32(m.Frames), MovedFrames: uint32(m.MovedFrames),
			MaxChange: float32(math.Round(m.MaxChange*1000) / 1000), Moved: m.Moved(tr.Params().Motion), WatchedS: float32(math.Round(m.WatchedS()*10) / 10)}
	}
	// Every part seen during a hover carries its task (casualty-analyst
	// registers a hover's records together), watched for movement or not.
	d.TaskId = t.TaskID
	if ev := t.Best; ev != nil {
		d.CaptureUnixNs, d.FrameId, d.SimTimeS, d.RangeM = ev.CaptureNS, ev.FrameID, ev.SimTimeS, float32(math.Round(ev.RangeM*10)/10)
		d.CapturePose = &sarpb.CapturePose{Position: &sarpb.GeoPoint{LatDeg: ev.Pose.Lat, LonDeg: ev.Pose.Lon, AltMslM: ev.Pose.AltMSL},
			AltAglM: float32(ev.Pose.AltAGL), YawDeg: float32(ev.Pose.YawDeg), PitchDeg: float32(ev.Pose.PitchDeg)}
		d.Outline = ev.Outline
		if len(ev.VisibleJPEG) > 0 {
			d.VisibleCrop = &sarpb.Crop{Format: "jpeg", Width: uint32(ev.VisW), Height: uint32(ev.VisH), Data: ev.VisibleJPEG, SourceBox: ev.VisBox[:]}
		}
		if len(ev.ThermalPNG) > 0 {
			d.ThermalCrop = &sarpb.Crop{Format: "png16-dk", Width: uint32(ev.ThW), Height: uint32(ev.ThH), Data: ev.ThermalPNG, SourceBox: ev.ThBox[:]}
		}
	}
	return d
}

func r1(v float64) float32 { return float32(math.Round(v*10) / 10) }

// Payload levels (sarpb.Payload values minus one).
const (
	LevelFull = iota
	LevelSmall
	LevelThermalOnly
	LevelMetadata
)

// Degrade returns a copy of a full packet reduced to a payload level:
// smaller visible crop, no visible crop, or no crops at all.
func Degrade(d *sarpb.Detection, level int) *sarpb.Detection {
	if level <= LevelFull {
		return d
	}
	out := proto.Clone(d).(*sarpb.Detection)
	switch {
	case level == LevelSmall:
		out.Payload = sarpb.Payload_PAYLOAD_SMALL
		if c := out.VisibleCrop; c != nil {
			if img, err := jpeg.Decode(bytes.NewReader(c.Data)); err == nil {
				small := shrink(fromImage(img), CropSmall)
				c.Data, c.Width, c.Height = encodeJPEG(small, jpegSmall), uint32(small.W), uint32(small.H)
			}
		}
	case level == LevelThermalOnly:
		out.Payload = sarpb.Payload_PAYLOAD_THERMAL_ONLY
		out.VisibleCrop = nil
	default:
		out.Payload = sarpb.Payload_PAYLOAD_METADATA
		out.VisibleCrop, out.ThermalCrop = nil, nil
	}
	return out
}

func fromImage(img image.Image) *raster.RGB {
	b := img.Bounds()
	out := raster.NewRGB(b.Dx(), b.Dy())
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			out.Set(x, y, [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)})
		}
	}
	return out
}
