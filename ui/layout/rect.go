package layout

// Point is a screen position in terminal cell coordinates (column X, row Y).
type Point struct {
	X int
	Y int
}

// Rect is an axis-aligned region of the terminal in cell coordinates. W and
// H are sizes, not inclusive corners: the rect spans columns [X, X+W) and
// rows [Y, Y+H).
type Rect struct {
	X int
	Y int
	W int
	H int
}

// Empty reports whether the rect covers no cells.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Right returns the exclusive right edge (first column past the rect).
func (r Rect) Right() int { return r.X + r.W }

// Bottom returns the exclusive bottom edge (first row past the rect).
func (r Rect) Bottom() int { return r.Y + r.H }

// Contains reports whether p falls inside the rect. Edges are half-open:
// (X, Y) is inside, (Right(), Bottom()) is not.
func (r Rect) Contains(p Point) bool {
	return p.X >= r.X && p.X < r.Right() && p.Y >= r.Y && p.Y < r.Bottom()
}

// Local translates a screen point into rect-local coordinates.
func (r Rect) Local(p Point) Point { return Point{X: p.X - r.X, Y: p.Y - r.Y} }

// Intersects reports whether two rects share at least one cell. Empty rects
// intersect nothing.
func (r Rect) Intersects(o Rect) bool {
	if r.Empty() || o.Empty() {
		return false
	}
	return r.X < o.Right() && o.X < r.Right() && r.Y < o.Bottom() && o.Y < r.Bottom()
}

// CutLeft splits the rect into a left band of w columns and the remainder.
// w is clamped to [0, r.W], so the two parts always tile r exactly.
func (r Rect) CutLeft(w int) (left, rem Rect) {
	w = clampInt(w, 0, r.W)
	left = Rect{X: r.X, Y: r.Y, W: w, H: r.H}
	rem = Rect{X: r.X + w, Y: r.Y, W: r.W - w, H: r.H}
	return left, rem
}

// CutTop splits the rect into a top band of h rows and the remainder. h is
// clamped to [0, r.H], so the two parts always tile r exactly.
func (r Rect) CutTop(h int) (top, rem Rect) {
	h = clampInt(h, 0, r.H)
	top = Rect{X: r.X, Y: r.Y, W: r.W, H: h}
	rem = Rect{X: r.X, Y: r.Y + h, W: r.W, H: r.H - h}
	return top, rem
}

// CutBottom splits the rect into the remainder and a bottom band of h rows.
// h is clamped to [0, r.H], so the two parts always tile r exactly.
func (r Rect) CutBottom(h int) (rem, bottom Rect) {
	h = clampInt(h, 0, r.H)
	rem = Rect{X: r.X, Y: r.Y, W: r.W, H: r.H - h}
	bottom = Rect{X: r.X, Y: r.Y + r.H - h, W: r.W, H: h}
	return rem, bottom
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
