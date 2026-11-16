package geometry

import (
	"atlas-render/internal/model"
	"math"
	"math/rand"
	"testing"
)

const tolerance = 1e-9

func closeTo(a, b float64) bool { return math.Abs(a-b) <= tolerance }

func TestFromAppliesLocalScaleThenRotationThenTranslation(t *testing.T) {
	cases := []model.Transform{
		{X: 11, Y: -7, ScaleX: 2, ScaleY: 1, Degrees: 0},
		{X: 0, Y: 0, ScaleX: 1, ScaleY: 1, Degrees: 90},
		{X: 4, Y: 9, ScaleX: 3, ScaleY: 0.5, Degrees: 45},
		{X: -12.5, Y: 3.25, ScaleX: 0.25, ScaleY: 4, Degrees: -137},
	}
	points := []model.Point{{}, {X: 1, Y: 0}, {X: 0, Y: 1}, {X: 7, Y: -3}}
	for _, tr := range cases {
		m := From(tr)
		radians := tr.Degrees * math.Pi / 180
		c, s := math.Cos(radians), math.Sin(radians)
		for _, p := range points {
			x, y := p.X*tr.ScaleX, p.Y*tr.ScaleY
			want := model.Point{X: tr.X + c*x - s*y, Y: tr.Y + s*x + c*y}
			got := m.Apply(p)
			if !closeTo(got.X, want.X) || !closeTo(got.Y, want.Y) {
				t.Fatalf("%+v applied to %+v = %+v, want %+v", tr, p, got, want)
			}
		}
	}
}

func TestCompositionEqualsSequentialApplication(t *testing.T) {
	random := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		parent := model.Transform{
			X: random.Float64()*200 - 100, Y: random.Float64()*200 - 100,
			ScaleX: 0.05 + random.Float64()*5, ScaleY: 0.05 + random.Float64()*5,
			Degrees: random.Float64()*720 - 360,
		}
		child := model.Transform{
			X: random.Float64()*200 - 100, Y: random.Float64()*200 - 100,
			ScaleX: 0.05 + random.Float64()*5, ScaleY: 0.05 + random.Float64()*5,
			Degrees: random.Float64()*720 - 360,
		}
		p := model.Point{X: random.Float64()*50 - 25, Y: random.Float64()*50 - 25}
		combined := From(parent).Mul(From(child))
		got := combined.Apply(p)
		want := From(parent).Apply(From(child).Apply(p))
		if !closeTo(got.X, want.X) || !closeTo(got.Y, want.Y) {
			t.Fatalf("composition disagrees with sequential application: %+v vs %+v", got, want)
		}
		// The composed matrix must also apply to the origin as the child's
		// translation mapped through the parent.
		origin := combined.Apply(model.Point{})
		parentOrigin := From(parent).Apply(model.Point{X: child.X, Y: child.Y})
		if !closeTo(origin.X, parentOrigin.X) || !closeTo(origin.Y, parentOrigin.Y) {
			t.Fatalf("translation composition: %+v vs %+v", origin, parentOrigin)
		}
	}
}

func TestInverseRoundTrips(t *testing.T) {
	random := rand.New(rand.NewSource(5))
	for i := 0; i < 200; i++ {
		m := From(model.Transform{
			X: random.Float64()*2000 - 1000, Y: random.Float64()*2000 - 1000,
			ScaleX: 0.001 + random.Float64()*100, ScaleY: 0.001 + random.Float64()*100,
			Degrees: random.Float64()*720 - 360,
		})
		inv, ok := m.Inverse()
		if !ok {
			t.Fatalf("legal transform reported singular: %+v", m)
		}
		p := model.Point{X: random.Float64()*4096 - 2048, Y: random.Float64()*4096 - 2048}
		round := inv.Apply(m.Apply(p))
		scale := math.Max(math.Abs(p.X), math.Abs(p.Y))
		if math.Abs(round.X-p.X) > 1e-6*math.Max(1, scale) || math.Abs(round.Y-p.Y) > 1e-6*math.Max(1, scale) {
			t.Fatalf("inverse did not round-trip %+v through %+v: %+v", p, m, round)
		}
	}
}

func TestQuarterTurnsAreExact(t *testing.T) {
	cases := map[float64]Matrix{
		0:    Identity(),
		90:   {A: 0, B: 1, C: -1, D: 0},
		180:  {A: -1, D: -1},
		270:  {A: 0, B: -1, C: 1, D: 0},
		360:  Identity(),
		-90:  {A: 0, B: -1, C: 1, D: 0},
		-180: {A: -1, D: -1},
		-270: {A: 0, B: 1, C: -1, D: 0},
		-360: Identity(),
		450:  {A: 0, B: 1, C: -1, D: 0},
		720:  Identity(),
		-720: Identity(),
	}
	for degrees, want := range cases {
		m := From(model.Transform{ScaleX: 1, ScaleY: 1, Degrees: degrees})
		if m != want {
			t.Fatalf("%g degrees produced %+v, want the exact matrix %+v", degrees, m, want)
		}
		inv, ok := m.Inverse()
		if !ok {
			t.Fatalf("%g degrees reported singular", degrees)
		}
		wantInverse := Matrix{A: want.A, B: want.C, C: want.B, D: want.D}
		if inv != wantInverse {
			t.Fatalf("%g degrees inverse = %+v, want %+v", degrees, inv, wantInverse)
		}
	}
	// While the sine is exact at quarter turns, a genuine fraction of a turn
	// must still rotate.
	diagonal := From(model.Transform{ScaleX: 1, ScaleY: 1, Degrees: 45})
	if !closeTo(diagonal.A, math.Sqrt2/2) || !closeTo(diagonal.B, math.Sqrt2/2) {
		t.Fatalf("45 degrees produced %+v", diagonal)
	}
}

func TestFullTurnKeepsPixelCenterEdge(t *testing.T) {
	// A rectangle whose right edge lies exactly on the center of pixel 110.
	// With a residual sin(2π) the edge moves by ~1e-14, but the inverse
	// transform can then classify that center as outside, deleting a legal
	// one-pixel column. The identity must include it.
	plain := From(model.Transform{X: 10.5, Y: 20.5, ScaleX: 1, ScaleY: 1})
	turned := From(model.Transform{X: 10.5, Y: 20.5, ScaleX: 1, ScaleY: 1, Degrees: 360})
	invPlain, _ := plain.Inverse()
	invTurned, _ := turned.Inverse()
	for _, x := range []float64{10.5, 11.5, 109.5, 110.5} {
		p := model.Point{X: x, Y: 30.5}
		a, b := invPlain.Apply(p), invTurned.Apply(p)
		if a != b {
			t.Fatalf("full turn changed local map at x=%g: %+v vs %+v", x, a, b)
		}
	}
}

func TestSingularAccumulatedTransformIsReported(t *testing.T) {
	tiny := From(model.Transform{ScaleX: 0.001, ScaleY: 0.001})
	m := tiny.Mul(tiny).Mul(tiny)
	if _, ok := m.Inverse(); ok {
		t.Fatalf("1e-9 scale product should be rejected as singular: %+v", m)
	}
}

func TestInRectUsesHalfOpenEdges(t *testing.T) {
	r := model.Rect{X: 2, Y: 3, Width: 4, Height: 5}
	inside := []model.Point{{X: 2, Y: 3}, {X: 2, Y: 7.999}, {X: 5.999, Y: 3}, {X: 5.999, Y: 7.999}}
	for _, p := range inside {
		if !InRect(p, r) {
			t.Fatalf("%+v should be inside %+v", p, r)
		}
	}
	outside := []model.Point{{X: 1.999, Y: 3}, {X: 2, Y: 2.999}, {X: 6, Y: 3}, {X: 2, Y: 8}, {X: 6, Y: 8}}
	for _, p := range outside {
		if InRect(p, r) {
			t.Fatalf("%+v should be outside %+v", p, r)
		}
	}
}

func TestInPolygonUsesEvenOddRule(t *testing.T) {
	square := []model.Point{{X: 0, Y: 0}, {X: 4, Y: 0}, {X: 4, Y: 4}, {X: 0, Y: 4}}
	for _, p := range []model.Point{{X: 2, Y: 2}, {X: 0.5, Y: 3.5}} {
		if !InPolygon(p, square) {
			t.Fatalf("%+v should be inside the square", p)
		}
	}
	for _, p := range []model.Point{{X: -0.5, Y: 2}, {X: 4.5, Y: 2}, {X: 2, Y: -0.5}, {X: 2, Y: 4.5}} {
		if InPolygon(p, square) {
			t.Fatalf("%+v should be outside the square", p)
		}
	}
	// Two nested windings cancel under the even-odd rule.
	ring := []model.Point{{X: 0, Y: 0}, {X: 6, Y: 0}, {X: 6, Y: 6}, {X: 0, Y: 6}, {X: 2, Y: 2}, {X: 2, Y: 4}, {X: 4, Y: 4}, {X: 4, Y: 2}}
	if InPolygon(model.Point{X: 3, Y: 3}, ring) {
		t.Fatal("point inside the hole should be outside under even-odd")
	}
}
