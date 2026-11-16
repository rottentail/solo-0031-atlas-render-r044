package raster

import (
	"atlas-render/internal/model"
	"context"
	"image"
	"image/color"
	"math"
	"testing"
)

func maskAt(img image.Image, bg color.NRGBA) []bool {
	out := make([]bool, img.Bounds().Dx()*img.Bounds().Dy())
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			out[y*img.Bounds().Dx()+x] = color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) != bg
		}
	}
	return out
}

func solidRect(id string, width, height float64, tr model.Transform) model.Layer {
	return model.Layer{ID: id, Kind: "rectangle", Width: width, Height: height, Transform: tr,
		Opacity: 1, Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}}
}

func sceneOf(layers ...model.Layer) model.Document {
	return model.Document{Width: 320, Height: 240, Background: "#000000", Layers: layers}
}

// TestOutputScaleIsSamplingDensity pins the documented meaning of output
// scale: scale 2 renders the same world at twice the sampling density. When
// rendered edges stay axis aligned, every scale-1 pixel becomes an exact 2x2
// block, so the two masks must agree under block replication. A coordinate
// error in either scale shows up as a shifted or dropped block. (Rotated
// geometry is excluded on purpose: pixel-center sampling of a slanted edge is
// not block-replicated, which is a sampling property, not a coordinate one.)
func TestOutputScaleIsSamplingDensity(t *testing.T) {
	cases := map[string]model.Document{
		"integer-edge":      sceneOf(solidRect("a", 100, 60, model.Transform{X: 10, Y: 20, ScaleX: 1, ScaleY: 1})),
		"quarter-turn":      sceneOf(solidRect("a", 100, 60, model.Transform{X: 200, Y: 40, ScaleX: 1, ScaleY: 1, Degrees: 90})),
		"non-uniform":       sceneOf(solidRect("a", 40, 20, model.Transform{X: 30, Y: 50, ScaleX: 3, ScaleY: 0.5})),
		"nested-clip":       clipScene(),
		"fractional-office": sceneOf(solidRect("a", 50, 30, model.Transform{X: 10.25, Y: 20, ScaleX: 1, ScaleY: 1})),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			one := maskAt(renderDocument(t, document, 1), background(document))
			two := maskAt(renderDocument(t, document, 2), background(document))
			width := document.Width
			height := document.Height
			for y := 0; y < height; y++ {
				for x := 0; x < width; x++ {
					want := one[y*width+x]
					for dy := 0; dy < 2; dy++ {
						for dx := 0; dx < 2; dx++ {
							got := two[(2*y+dy)*(2*width)+2*x+dx]
							if got != want {
								t.Fatalf("scale 2 block (%d,%d)+(%d,%d) is %v while scale 1 pixel is %v; "+
									"output scale must refine sampling, not move geometry", x, y, dx, dy, got, want)
							}
						}
					}
				}
			}
		})
	}
}

func clipScene() model.Document {
	outer := model.Layer{ID: "outer", Kind: "group", Opacity: 1,
		Transform: model.Transform{X: 100, Y: 60, ScaleX: 1, ScaleY: 1},
		Clip:      &model.Rect{X: 20, Y: 10, Width: 80, Height: 70}}
	inner := model.Layer{ID: "inner", Kind: "group", Opacity: 1,
		Transform: model.Transform{X: 10, Y: 5, ScaleX: 1, ScaleY: 1},
		Clip:      &model.Rect{X: 0, Y: 0, Width: 60, Height: 30}}
	inner.Children = []model.Layer{solidRect("fill", 400, 400, model.Transform{ScaleX: 1, ScaleY: 1})}
	outer.Children = []model.Layer{inner}
	return sceneOf(outer)
}

func rotatedClipScene() model.Document {
	frame := model.Layer{ID: "frame", Kind: "group", Opacity: 1,
		Transform: model.Transform{X: 80, Y: 50, ScaleX: 1.5, ScaleY: 0.75, Degrees: 25},
		Clip:      &model.Rect{X: 20, Y: 10, Width: 120, Height: 100}}
	frame.Children = []model.Layer{solidRect("fill", 400, 300, model.Transform{ScaleX: 1, ScaleY: 1})}
	return sceneOf(frame)
}

// TestFullTurnIsIdentity checks the group property directly: adding a whole
// number of turns to any layer must not change a single pixel. The naive
// math.Cos(2*pi) leaves a 2.4e-16 rotation, which moves edge-on-center pixels.
func TestFullTurnIsIdentity(t *testing.T) {
	// Only quarter turns are required to be exact. For a general angle,
	// cos(137.25°) and cos(497.25°) legitimately differ in the last bit, so a
	// whole-turn shift may move a boundary pixel by sub-pixel sampling; that is
	// floating-point reality, not a coordinate bug. Quarter turns, by
	// contrast, are exact group elements and must be bit-identical.
	angles := []float64{0, 90, 180, 270, -90, -180, -270}
	for _, angle := range angles {
		for _, turns := range []float64{360, 720, -360, -720} {
			base := sceneOf(solidRect("a", 100, 60, model.Transform{X: 10.5, Y: 20.5, ScaleX: 1, ScaleY: 1, Degrees: angle}))
			shift := sceneOf(solidRect("a", 100, 60, model.Transform{X: 10.5, Y: 20.5, ScaleX: 1, ScaleY: 1, Degrees: angle + turns}))
			first := maskAt(renderDocument(t, base, 1), background(base))
			second := maskAt(renderDocument(t, shift, 1), background(shift))
			for i := range first {
				if first[i] != second[i] {
					t.Fatalf("angle %g versus %g differs at device (%d,%d); whole turns must be exact",
						angle, angle+turns, i%base.Width, i/base.Width)
				}
			}
		}
	}
}

// TestLegalEdgeOnPixelCenterSurvives requires a rectangle whose edges lie
// exactly on pixel centers to keep its full area. The inverse transform is
// evaluated at those centers, so a residual epsilon in the rotation would
// classify the boundary columns as outside and silently drop them. Only whole
// turns leave the rectangle in place; a quarter turn legitimately relocates it.
func TestLegalEdgeOnPixelCenterSurvives(t *testing.T) {
	for _, degrees := range []float64{0, 360, -360, 720, -720} {
		document := sceneOf(solidRect("edge", 100, 60,
			model.Transform{X: 10.5, Y: 20.5, ScaleX: 1, ScaleY: 1, Degrees: degrees}))
		for _, scale := range []int{1, 2} {
			img := renderDocument(t, document, scale)
			stats := measure(img, background(document))
			want := 100 * 60 * scale * scale
			if stats.Covered != want {
				t.Fatalf("angle %g scale %d covered %d pixels, want the legal area %d "+
					"(edge on pixel center must not vanish)", degrees, scale, stats.Covered, want)
			}
		}
	}
}

// TestAncestorClipBoundsDescendant requires an oversized descendant to be
// bounded exactly by the intersection of every clip in its ancestry. Adding a
// stricter ancestor clip may only shrink coverage, never grow it, and the
// surviving region must match the analytic intersection.
func TestAncestorClipBoundsDescendant(t *testing.T) {
	document := clipScene()
	stats := measure(renderDocument(t, document, 1), background(document))
	// Outer world x [120,200) y [70,140); inner world x [110,170) y [65,95);
	// intersection x [120,170) y [70,95) = 50x25.
	if stats.Covered != 50*25 {
		t.Fatalf("nested clip coverage %d, want the intersection 1250", stats.Covered)
	}
	if stats.MinX != 120 || stats.MaxX != 169 || stats.MinY != 70 || stats.MaxY != 94 {
		t.Fatalf("nested clip bbox [%d,%d..%d,%d], want [120,70..169,94]",
			stats.MinX, stats.MinY, stats.MaxX, stats.MaxY)
	}

	loose := clipScene()
	loose.Layers[0].Children[0].Clip = &model.Rect{X: 0, Y: 0, Width: 200, Height: 200}
	looseStats := measure(renderDocument(t, loose, 1), background(loose))
	if looseStats.Covered < stats.Covered {
		t.Fatalf("loosening the descendant clip reduced coverage: %d < %d", looseStats.Covered, stats.Covered)
	}
	if looseStats.Covered != 80*70 {
		t.Fatalf("with only the outer clip coverage %d, want the outer region 5600", looseStats.Covered)
	}
}

// TestClipTranslationEquivariance shifts a clip and its owning group by an
// integer in world coordinates; the painted region must shift by exactly the
// same integer at scale 1 and twice that at scale 2. This catches clips
// evaluated in the wrong frame (e.g. child-local instead of ancestor-local).
func TestClipTranslationEquivariance(t *testing.T) {
	build := func(dx, dy int) model.Document {
		frame := model.Layer{ID: "frame", Kind: "group", Opacity: 1,
			Transform: model.Transform{X: float64(50 + dx), Y: float64(40 + dy), ScaleX: 1, ScaleY: 1},
			Clip:      &model.Rect{X: 10, Y: 10, Width: 30, Height: 20}}
		frame.Children = []model.Layer{solidRect("fill", 200, 200, model.Transform{ScaleX: 1, ScaleY: 1})}
		return sceneOf(frame)
	}
	base := build(0, 0)
	for _, scale := range []int{1, 2} {
		baseStats := measure(renderDocument(t, base, scale), background(base))
		for _, delta := range [][2]int{{7, 0}, {0, 11}, {-9, 5}} {
			moved := build(delta[0], delta[1])
			stats := measure(renderDocument(t, moved, scale), background(moved))
			if stats.Covered != baseStats.Covered {
				t.Fatalf("shifting the clip changed coverage %d -> %d", baseStats.Covered, stats.Covered)
			}
			if stats.MinX-baseStats.MinX != delta[0]*scale || stats.MinY-baseStats.MinY != delta[1]*scale {
				t.Fatalf("scale %d shift %v moved the region by (%d,%d), want (%d,%d)",
					scale, delta, stats.MinX-baseStats.MinX, stats.MinY-baseStats.MinY,
					delta[0]*scale, delta[1]*scale)
			}
		}
	}
}

// TestRotationPreservesAreaUnderNonUniformScale converts a rectangle to an
// equivalent one along swapped axes and requires matching coverage. A 90-degree
// turn must exchange the two scale factors and the two dimensions; if the
// matrix multiplies in the wrong order this relation breaks.
func TestRotationPreservesAreaUnderNonUniformScale(t *testing.T) {
	// Local 40x20 scaled by (2,3) and turned 90 degrees occupies a 60x80 box.
	turned := sceneOf(solidRect("a", 40, 20,
		model.Transform{X: 140, Y: 40, ScaleX: 2, ScaleY: 3, Degrees: 90}))
	// The equivalent axis-aligned rectangle: local 20x40 scaled by (3,2) places
	// a 60x80 box at (80,40), exactly where the turned one lands.
	equivalent := sceneOf(solidRect("a", 20, 40,
		model.Transform{X: 80, Y: 40, ScaleX: 3, ScaleY: 2}))
	for _, scale := range []int{1, 2} {
		got := measure(renderDocument(t, turned, scale), background(turned))
		want := measure(renderDocument(t, equivalent, scale), background(equivalent))
		if got.Covered != want.Covered || got.MinX != want.MinX || got.MinY != want.MinY ||
			got.MaxX != want.MaxX || got.MaxY != want.MaxY {
			t.Fatalf("scale %d: rotated %+v, equivalent %+v", scale, got, want)
		}
	}
}

// TestClipAndScaleVaryTogether is the headline combination: rotation,
// non-uniform scale, an ancestor clip and the output multiplier all change at
// once. Coverage must be the mapped clip area and the centroid must track the
// mapped clip center; a stale scale or wrong clip frame moves the centroid.
func TestClipAndScaleVaryTogether(t *testing.T) {
	document := rotatedClipScene()
	area := 120 * 100 * 1.5 * 0.75
	for _, scale := range []int{1, 2} {
		stats := measure(renderDocument(t, document, scale), background(document))
		if math.Abs(float64(stats.Covered)-area*float64(scale*scale)) > 36 {
			t.Fatalf("scale %d coverage %d, want clip area %.1f (sampling tolerance 36)",
				scale, stats.Covered, area*float64(scale*scale))
		}
	}
	one := measure(renderDocument(t, document, 1), background(document))
	two := measure(renderDocument(t, document, 2), background(document))
	if math.Abs(two.Centroid[0]-2*one.Centroid[0]) > 1.5 || math.Abs(two.Centroid[1]-2*one.Centroid[1]) > 1.5 {
		t.Fatalf("centroid did not double with output scale: %+v versus %+v", one, two)
	}
}

// TestTransparentLayerNeverOpaquesBackground documents that opacity multiplies
// the layer alpha only; a zero-opacity layer must leave pixels untouched.
func TestTransparentLayerNeverOpaquesBackground(t *testing.T) {
	document := sceneOf(model.Layer{ID: "ghost", Kind: "rectangle", Width: 100, Height: 60,
		Transform: model.Transform{X: 10, Y: 10, ScaleX: 1, ScaleY: 1}, Opacity: 0,
		Fill: model.Paint{Kind: "solid", Color: "#FFFFFF"}})
	img := renderDocument(t, document, 1)
	if stats := measure(img, background(document)); stats.Covered != 0 {
		t.Fatalf("zero-opacity layer painted %d pixels", stats.Covered)
	}
}

// TestCancelledContextStopsRender proves the renderer observes cancellation
// between rows instead of running to completion, which is what lets the
// engine honour its cancellation deadline.
func TestCancelledContextStopsRender(t *testing.T) {
	document := model.Document{Width: 512, Height: 512, Background: "#000000",
		Layers: []model.Layer{solidRect("big", 512, 512, model.Transform{ScaleX: 1, ScaleY: 1})}}
	plan, err := Compile(model.Snapshot{Document: document, Assets: map[string]model.Asset{}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plan.Render(ctx); err == nil {
		t.Fatal("render completed despite an already cancelled context")
	}
}
