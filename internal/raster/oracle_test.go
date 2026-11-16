package raster

import (
	"atlas-render/internal/model"
	"fmt"
	"image/color"
	"math"
	"math/rand"
	"testing"
)

// oracle is an independent containment predicate written straight from the
// specification: compose the ancestor chain, invert it, and test the mapped
// point against each half-open local rectangle and clip in that frame. It
// shares no code with Compile or Render, so agreement is evidence about the
// production path rather than a restatement of it.
type oracle struct {
	transform model.Transform
	width     float64
	height    float64
	group     bool
	disc      bool
	clip      *model.Rect
	kids      []oracle
}

func (o oracle) contains(world model.Point) bool {
	local := localPoint(o.transform, world)
	if !o.group {
		if local.X < 0 || local.Y < 0 || local.X >= o.width || local.Y >= o.height {
			return false
		}
		if o.disc {
			dx, dy := 2*local.X/o.width-1, 2*local.Y/o.height-1
			if dx*dx+dy*dy > 1 {
				return false
			}
		}
	}
	if o.clip != nil {
		r := *o.clip
		if local.X < r.X || local.Y < r.Y || local.X >= r.X+r.Width || local.Y >= r.Y+r.Height {
			return false
		}
	}
	for _, kid := range o.kids {
		if !kid.contains(local) {
			return false
		}
	}
	return true
}

// localPoint inverts one layer transform: subtract translation, undo rotation,
// then undo local scale. Quarter turns are exact because the documented
// semantics treat them as exact group elements (and because a residual
// sin(2*pi) would otherwise classify pixel-center edges differently).
func localPoint(t model.Transform, p model.Point) model.Point {
	quarter := t.Degrees / 90
	var c, s float64
	if quarter == math.Trunc(quarter) {
		c, s = []float64{1, 0, -1, 0}[int(math.Mod(quarter, 4)+4)%4],
			[]float64{0, 1, 0, -1}[int(math.Mod(quarter, 4)+4)%4]
	} else {
		radians := t.Degrees * math.Pi / 180
		c, s = math.Cos(radians), math.Sin(radians)
	}
	dx, dy := p.X-t.X, p.Y-t.Y
	return model.Point{X: (c*dx + s*dy) / t.ScaleX, Y: (-s*dx + c*dy) / t.ScaleY}
}

func (o oracle) layer(id string) model.Layer {
	layer := model.Layer{ID: id, Kind: "rectangle", Width: o.width, Height: o.height,
		Transform: o.transform, Opacity: 1, Clip: o.clip,
		Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}}
	if o.disc {
		layer.Kind = "ellipse"
	}
	if o.group {
		layer.Kind, layer.Width, layer.Height = "group", 0, 0
		for i, kid := range o.kids {
			layer.Children = append(layer.Children, kid.layer(fmt.Sprintf("%s-%d", id, i)))
		}
	}
	return layer
}

func randomOracle(r *rand.Rand, depth int) oracle {
	o := oracle{
		transform: model.Transform{
			X: r.Float64() * 260, Y: r.Float64() * 180,
			ScaleX: 0.2 + r.Float64()*3, ScaleY: 0.2 + r.Float64()*3,
			Degrees: float64(r.Intn(720) - 360),
		},
		width: 20 + r.Float64()*160, height: 20 + r.Float64()*160,
	}
	if r.Intn(3) != 0 {
		o.clip = &model.Rect{X: -10 + r.Float64()*20, Y: -10 + r.Float64()*20,
			Width: 10 + r.Float64()*100, Height: 10 + r.Float64()*100}
	}
	if r.Intn(4) == 0 {
		o.disc = true
	}
	if depth > 0 && r.Intn(2) == 0 {
		o.group = true
		o.width, o.height = 0, 0
		kid := randomOracle(r, depth-1)
		kid.transform.X, kid.transform.Y = kid.transform.X/4, kid.transform.Y/4
		o.kids = []oracle{kid}
	}
	return o
}

// TestRandomizedGeometryAgainstIndependentOracle checks every device pixel of
// randomized scenes against the oracle at both output scales. It covers
// rotation combined with non-uniform scale, nested ancestor clips, group
// hierarchy and the output scale factor moving together.
func TestRandomizedGeometryAgainstIndependentOracle(t *testing.T) {
	r := rand.New(rand.NewSource(20261116))
	for i := 0; i < 120; i++ {
		scene := randomOracle(r, 2)
		document := model.Document{Width: 200, Height: 150, Background: "#000000",
			Layers: []model.Layer{scene.layer("root")}}
		for _, scale := range []int{1, 2} {
			img := renderDocument(t, document, scale)
			bg := background(document)
			miss, extra := 0, 0
			firstMiss, firstExtra := "", ""
			for y := 0; y < document.Height*scale; y++ {
				for x := 0; x < document.Width*scale; x++ {
					world := model.Point{X: (float64(x) + 0.5) / float64(scale), Y: (float64(y) + 0.5) / float64(scale)}
					want := scene.contains(world)
					got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) != bg
					if want && !got {
						miss++
						if firstMiss == "" {
							firstMiss = fmt.Sprintf("device(%d,%d) world(%.3f,%.3f)", x, y, world.X, world.Y)
						}
					}
					if !want && got {
						extra++
						if firstExtra == "" {
							firstExtra = fmt.Sprintf("device(%d,%d) world(%.3f,%.3f)", x, y, world.X, world.Y)
						}
					}
				}
			}
			if miss+extra != 0 {
				t.Fatalf("scene %d scale %d: %d required pixels missing, %d illegal pixels painted\nmissing at %s\nextra at %s",
					i, scale, miss, extra, firstMiss, firstExtra)
			}
		}
	}
}
