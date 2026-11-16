package geometry

import (
	"atlas-render/internal/model"
	"math"
)

type Matrix struct{ A, B, C, D, E, F float64 }

func Identity() Matrix { return Matrix{A: 1, D: 1} }
func From(t model.Transform) Matrix {
	c, s := cosSin(t.Degrees)
	return Matrix{A: c * t.ScaleX, B: s * t.ScaleX, C: -s * t.ScaleY, D: c * t.ScaleY, E: t.X, F: t.Y}
}

// cosSin returns the cosine and sine of an angle in degrees. Quarter turns are
// answered exactly so that a full turn composes to the identity; relying on
// math.Cos(2π) leaves ~2.4e-16 of rotation, which moves axis-aligned edges that
// sit exactly on an output pixel center by half a pixel.
func cosSin(degrees float64) (float64, float64) {
	q := degrees / 90
	if q == math.Trunc(q) {
		switch int(math.Mod(q, 4)+4) % 4 {
		case 0:
			return 1, 0
		case 1:
			return 0, 1
		case 2:
			return -1, 0
		default:
			return 0, -1
		}
	}
	r := degrees * math.Pi / 180
	return math.Cos(r), math.Sin(r)
}
func (m Matrix) Mul(n Matrix) Matrix {
	return Matrix{A: m.A*n.A + m.C*n.B, B: m.B*n.A + m.D*n.B, C: m.A*n.C + m.C*n.D, D: m.B*n.C + m.D*n.D, E: m.A*n.E + m.C*n.F + m.E, F: m.B*n.E + m.D*n.F + m.F}
}
func (m Matrix) Apply(p model.Point) model.Point {
	return model.Point{X: m.A*p.X + m.C*p.Y + m.E, Y: m.B*p.X + m.D*p.Y + m.F}
}
func (m Matrix) Inverse() (Matrix, bool) {
	d := m.A*m.D - m.B*m.C
	if math.Abs(d) < 1e-12 {
		return Matrix{}, false
	}
	n := Matrix{A: m.D / d, B: -m.B / d, C: -m.C / d, D: m.A / d}
	n.E = -n.A*m.E - n.C*m.F
	n.F = -n.B*m.E - n.D*m.F
	return n, true
}
func InRect(p model.Point, r model.Rect) bool {
	return p.X >= r.X && p.Y >= r.Y && p.X < r.X+r.Width && p.Y < r.Y+r.Height
}
func InPolygon(p model.Point, points []model.Point) bool {
	inside := false
	j := len(points) - 1
	for i := range points {
		a, b := points[i], points[j]
		if (a.Y > p.Y) != (b.Y > p.Y) && p.X < (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y)+a.X {
			inside = !inside
		}
		j = i
	}
	return inside
}
