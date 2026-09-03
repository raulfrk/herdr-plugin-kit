// Package responsive resolves terminal dimensions into bounded render layouts.
package responsive

const (
	maxCols = 500
	maxRows = 200
)

// Size is a terminal size measured in character cells.
type Size struct {
	Columns int
	Rows    int
}

// Class identifies the responsive layout selected for a terminal size.
type Class string

const (
	Recovery Class = "recovery"
	Compact  Class = "compact"
	Standard Class = "standard"
	Wide     Class = "wide"
)

// Layout contains the responsive class and the dimensions safe to render.
type Layout struct {
	Reported   Size
	Render     Size
	Class      Class
	Functional bool
	Projected  bool
}

// Resolve classifies size and bounds each render dimension independently.
func Resolve(size Size) Layout {
	class := Wide
	if size.Columns < 40 || size.Rows < 10 {
		class = Recovery
	} else if size.Columns < 80 || size.Rows < 18 {
		class = Compact
	} else if size.Columns < 110 || size.Rows < 24 {
		class = Standard
	}

	return Layout{
		Reported: size,
		Render: Size{
			Columns: clampDimension(size.Columns, maxCols),
			Rows:    clampDimension(size.Rows, maxRows),
		},
		Class:      class,
		Functional: class != Recovery && size.Columns <= maxCols && size.Rows <= maxRows,
		Projected:  size.Columns > maxCols || size.Rows > maxRows,
	}
}

func clampDimension(value, maximum int) int {
	if value < 1 {
		return 0
	}
	return min(value, maximum)
}
