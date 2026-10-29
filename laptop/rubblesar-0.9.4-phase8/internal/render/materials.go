// Package render turns a scenario into per-site raster "layers": colour,
// height, body-part labels, victim ownership and the thermal ingredients
// (skin coupling, solar absorptivity, fixed heat offsets). The virtual camera
// (package vcam) samples these layers to produce RGB and thermal frames for
// any drone pose and time.
//
// The renderer is a 2.5D procedural model (height field + top-down colour),
// chosen because it is deterministic, pure Go, fast enough for the Jetson and
// testable. It is not photorealistic; Blender-based realism can be added later
// without changing the layer format.
package render

// Material ids used during rendering (stored only inside the renderer).
const (
	matGround uint8 = iota
	matDust
	matConcrete
	matConcreteDark
	matBrick
	matPlaster
	matTimber
	matRebar
	matTile
	matGlass
	matFabric
	matFoam
	matMetalSheet
	matCharred
	matSkinLike
	matSkin
	matCloth
	matHair
	matShoe
)

type material struct {
	rgb    [3]float64
	vary   float64 // per-object brightness variation
	grain  float64 // per-pixel noise amplitude
	absorb float64 // solar absorptivity 0..1
}

var materials = map[uint8]material{
	matGround:       {[3]float64{126, 120, 110}, 0.05, 0.06, 0.75},
	matDust:         {[3]float64{178, 170, 156}, 0.06, 0.05, 0.55},
	matConcrete:     {[3]float64{160, 158, 152}, 0.10, 0.07, 0.60},
	matConcreteDark: {[3]float64{112, 110, 106}, 0.10, 0.07, 0.70},
	matBrick:        {[3]float64{146, 86, 66}, 0.12, 0.08, 0.72},
	matPlaster:      {[3]float64{206, 200, 188}, 0.06, 0.05, 0.40},
	matTimber:       {[3]float64{122, 88, 58}, 0.15, 0.10, 0.75},
	matRebar:        {[3]float64{82, 58, 44}, 0.10, 0.08, 0.90},
	matTile:         {[3]float64{168, 100, 78}, 0.10, 0.06, 0.70},
	matGlass:        {[3]float64{150, 170, 172}, 0.10, 0.10, 0.30},
	matFoam:         {[3]float64{214, 190, 120}, 0.08, 0.05, 0.50},
	matMetalSheet:   {[3]float64{58, 64, 76}, 0.10, 0.04, 0.95},
	matCharred:      {[3]float64{44, 38, 34}, 0.10, 0.10, 0.95},
	matSkinLike:     {[3]float64{214, 170, 140}, 0.08, 0.04, 0.60},
	matShoe:         {[3]float64{44, 38, 34}, 0.25, 0.05, 0.85},
}

// Fabric fragments (furniture, curtains, bedding) in debris.
var fabricColours = [][3]float64{
	{140, 40, 44}, {46, 70, 120}, {70, 110, 70}, {190, 160, 90}, {120, 60, 110}, {200, 200, 196}, {60, 60, 64},
}

// Palettes indexed by scenario.Appearance.
var (
	skinTones = [][3]float64{
		{233, 196, 168}, {224, 172, 138}, {198, 134, 100}, {161, 102, 72}, {120, 78, 55}, {88, 58, 42},
	}
	clothColours = [][3]float64{
		{38, 48, 84}, {70, 70, 74}, {150, 36, 40}, {84, 92, 52}, {176, 160, 128}, {64, 92, 140}, {28, 28, 30}, {110, 36, 56},
	}
	hairColours = [][3]float64{{24, 20, 18}, {58, 40, 28}, {104, 74, 50}, {150, 146, 140}}
)

// Thermal coupling of body materials to the victim's skin temperature.
const (
	coupleSkin  = 255
	coupleCloth = 140
	coupleHair  = 115
	coupleShoe  = 70
	coupleBleed = 38 // debris touching a body, at contact
	coupleOccl  = 26 // debris lying on a body
)

var dustColour = [3]float64{184, 176, 162}
