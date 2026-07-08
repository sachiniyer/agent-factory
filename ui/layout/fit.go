package layout

// FitContentRect returns dimensions that fit inside maxOuter after the caller
// adds the supplied outer frame cells. For lipgloss styles, pass border sizes:
// Style.Width/Height already include padding, but rendered output adds borders.
// A non-positive preferred dimension means "use all available space"; a
// non-positive maxOuter dimension means "unbounded" for that axis.
func FitContentRect(preferred, maxOuter Rect, horizontalFrame, verticalFrame int) Rect {
	return Rect{
		W: fitContentDimension(preferred.W, maxOuter.W, horizontalFrame),
		H: fitContentDimension(preferred.H, maxOuter.H, verticalFrame),
	}
}

func fitContentDimension(preferred, maxOuter, frame int) int {
	if maxOuter <= 0 {
		if preferred > 0 {
			return preferred
		}
		return 0
	}
	available := maxOuter - frame
	if available < 1 {
		available = 1
	}
	if preferred <= 0 || preferred > available {
		return available
	}
	return preferred
}
