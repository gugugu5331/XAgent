package tui

const (
	BaselineWidth  = 80
	BaselineHeight = 24

	// MaxReadableWidth keeps long message, status, and input rows readable on
	// wide terminals while leaving the terminal itself available to later
	// renderers for centering.
	MaxReadableWidth = 120

	FullInputMaxHeight    = 8
	CompactInputMaxHeight = 3
	commandMenuHeight     = 5
	confirmationHeight    = 4
	statusHeight          = 1
)

type LayoutMode string

const (
	LayoutFull    LayoutMode = "full"
	LayoutCompact LayoutMode = "compact"
)

type Size struct {
	Width  int
	Height int
}

type Region struct {
	X      int
	Y      int
	Width  int
	Height int
}

func (region Region) Right() int  { return region.X + region.Width }
func (region Region) Bottom() int { return region.Y + region.Height }

type LayoutInput struct {
	Terminal         Size
	InputLines       int
	ShowConfirmation bool
	ShowCommandMenu  bool
	Screen           string
}

type Layout struct {
	Terminal     Size
	Mode         LayoutMode
	Main         Region
	CommandMenu  Region
	Confirmation Region
	Input        Region
	Status       Region
}

// ComputeLayout is the single geometry authority for TUI regions. It
// normalizes untrusted terminal and content measurements, then partitions the
// available height without overlap. Renderers may hide a zero-height region,
// but must not recompute its geometry independently.
func ComputeLayout(input LayoutInput) Layout {
	terminal := Size{
		Width:  nonNegative(input.Terminal.Width),
		Height: nonNegative(input.Terminal.Height),
	}
	mode := LayoutFull
	if terminal.Width < BaselineWidth || terminal.Height < BaselineHeight {
		mode = LayoutCompact
	}

	contentWidth := min(terminal.Width, MaxReadableWidth)
	contentX := (terminal.Width - contentWidth) / 2
	heights := allocateRegionHeights(input, terminal.Height, mode)

	y := 0
	main := Region{X: contentX, Y: y, Width: contentWidth, Height: heights.main}
	y = main.Bottom()
	menu := Region{X: contentX, Y: y, Width: contentWidth, Height: heights.commandMenu}
	y = menu.Bottom()
	confirmation := Region{X: contentX, Y: y, Width: contentWidth, Height: heights.confirmation}
	y = confirmation.Bottom()
	inputRegion := Region{X: contentX, Y: y, Width: contentWidth, Height: heights.input}
	y = inputRegion.Bottom()
	status := Region{X: contentX, Y: y, Width: contentWidth, Height: heights.status}

	return Layout{
		Terminal: terminal, Mode: mode, Main: main, CommandMenu: menu,
		Confirmation: confirmation, Input: inputRegion, Status: status,
	}
}

type regionHeights struct {
	main         int
	commandMenu  int
	confirmation int
	input        int
	status       int
}

func allocateRegionHeights(input LayoutInput, available int, mode LayoutMode) regionHeights {
	if available == 0 {
		return regionHeights{}
	}
	if input.Screen == string(ScreenList) {
		status := min(statusHeight, available)
		return regionHeights{main: available - status, status: status}
	}

	// Preserve one main row whenever the terminal has any height. Remaining
	// rows are assigned by user-impact priority; lower-priority panels collapse
	// to zero when the terminal is pathologically small.
	heights := regionHeights{main: 1}
	remaining := available - heights.main
	heights.status = takeHeight(&remaining, statusHeight)
	if input.ShowConfirmation {
		heights.confirmation = takeHeight(&remaining, confirmationHeight)
	}

	inputLimit := FullInputMaxHeight
	if mode == LayoutCompact {
		inputLimit = CompactInputMaxHeight
	}
	inputLines := input.InputLines
	if inputLines < 1 {
		inputLines = 1
	}
	heights.input = takeHeight(&remaining, min(inputLines, inputLimit))
	if input.ShowCommandMenu {
		heights.commandMenu = takeHeight(&remaining, commandMenuHeight)
	}
	heights.main += remaining
	return heights
}

func takeHeight(remaining *int, desired int) int {
	allocated := min(*remaining, nonNegative(desired))
	*remaining -= allocated
	return allocated
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
