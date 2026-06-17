package chat

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// contentLeftCol and contentTopRow are the screen position of the message
// content's top-left cell: content-text column 0 sits at screen column
// contentLeftCol, and the viewport's first visible row is screen row
// contentTopRow. They follow from the fixed sidebar width plus its border and
// the panes' padding; the values are measured against the real layout and
// pinned by TestCellAtMapsScreenToContent, which fails if the layout shifts.
const (
	contentLeftCol = sidebarWidth + 4
	contentTopRow  = 1
)

// selHighlightStyle paints a selected span: dark text on the selection colour.
var selHighlightStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(colSelect)

// selection is a text span over the rendered scrollback, from the anchor cell
// (where the drag began) to the current cell. Coordinates are content-absolute:
// row indexes the lines of renderConv's output, col is a terminal-cell column
// within the content text.
type selection struct {
	anchorRow, anchorCol int
	curRow, curCol       int
}

// normalized returns the span ordered top-left to bottom-right.
func (s selection) normalized() (r1, c1, r2, c2 int) {
	r1, c1, r2, c2 = s.anchorRow, s.anchorCol, s.curRow, s.curCol
	if r1 > r2 || (r1 == r2 && c1 > c2) {
		r1, c1, r2, c2 = r2, c2, r1, c1
	}
	return
}

// empty reports whether the span covers no cells (the cursor never left the
// anchor), i.e. a plain click rather than a drag.
func (s selection) empty() bool {
	return s.anchorRow == s.curRow && s.anchorCol == s.curCol
}

// beginSelection starts a drag selection at the pressed cell. A press outside
// the viewport rows is ignored.
func (m *Model) beginSelection(x, y int) {
	row, col, ok := m.cellAt(x, y)
	if !ok {
		return
	}
	m.sel = &selection{anchorRow: row, anchorCol: col, curRow: row, curCol: col}
	m.selDragging = true
	m.redrawSelection()
}

// extendSelection moves the drag's free end to the cursor cell (clamped into the
// viewport), redrawing the highlight.
func (m *Model) extendSelection(x, y int) {
	if m.sel == nil {
		return
	}
	m.sel.curRow, m.sel.curCol = m.cellAtClamped(x, y)
	m.redrawSelection()
}

// endSelection finishes a drag: an empty span (a click) just clears, otherwise
// the highlighted text is copied to the clipboard and left highlighted so the
// user sees what was taken.
func (m *Model) endSelection() tea.Cmd {
	m.selDragging = false
	if m.sel == nil {
		return nil
	}
	if m.sel.empty() {
		m.sel = nil
		m.redrawSelection()
		return nil
	}
	text := m.selectedText()
	m.redrawSelection()
	if strings.TrimSpace(text) == "" {
		return nil
	}
	m.statusNote = "copied selection to clipboard"
	return copyToClipboardCmd(text)
}

// clearSelection drops any selection and redraws if one was showing.
func (m *Model) clearSelection() {
	if m.sel == nil && !m.selDragging {
		return
	}
	m.sel = nil
	m.selDragging = false
	m.refreshViewport()
}

// redrawSelection re-renders the viewport with the current highlight without
// moving the scroll position, so the view stays put as the drag grows.
func (m *Model) redrawSelection() {
	if conv := m.active(); conv != nil {
		m.vp.SetContent(m.viewportContent(conv))
	}
}

// cellAt maps a screen position to an absolute content cell (row indexes
// renderConv's lines, col is a content-text column), reporting ok=false when the
// position is outside the message viewport's rows. The column is clamped at 0 so
// a press in the pane's left padding starts at the line's first cell.
func (m *Model) cellAt(x, y int) (row, col int, ok bool) {
	if y < contentTopRow || y >= contentTopRow+m.vp.Height {
		return 0, 0, false
	}
	return m.vp.YOffset + (y - contentTopRow), max(0, x-contentLeftCol), true
}

// cellAtClamped is cellAt with the position clamped into the viewport rows and
// content columns, for a drag that wanders past the pane's edges.
func (m *Model) cellAtClamped(x, y int) (row, col int) {
	y = min(max(y, contentTopRow), contentTopRow+m.vp.Height-1)
	return m.vp.YOffset + (y - contentTopRow), max(0, x-contentLeftCol)
}

// selectedText returns the plain text covered by the current selection: each
// touched content row sliced to the selected columns (first row from its start
// column, last row to its end column, whole rows between), joined by newlines
// with trailing pad-spaces trimmed.
func (m *Model) selectedText() string {
	conv := m.active()
	if conv == nil || m.sel == nil {
		return ""
	}
	lines := strings.Split(m.renderConv(conv), "\n")
	r1, c1, r2, c2 := m.sel.normalized()
	var out []string
	for row := max(r1, 0); row <= r2 && row < len(lines); row++ {
		plain := ansi.Strip(lines[row])
		start, end := 0, ansi.StringWidth(plain)
		if row == r1 {
			start = c1
		}
		if row == r2 {
			end = c2
		}
		out = append(out, strings.TrimRight(cutCols(plain, start, end), " "))
	}
	return strings.Join(out, "\n")
}

// applySelectionHighlight overlays sel on the rendered content. Lines the span
// touches are flattened to plain text with the selected cells painted in
// selHighlightStyle; untouched lines keep their styling. The span runs in
// reading order: the first row from its start column to the line end, whole rows
// between, and the last row up to its end column.
func applySelectionHighlight(content string, sel selection) string {
	r1, c1, r2, c2 := sel.normalized()
	lines := strings.Split(content, "\n")
	for row := max(r1, 0); row <= r2 && row < len(lines); row++ {
		plain := ansi.Strip(lines[row])
		start, end := 0, ansi.StringWidth(plain)
		if row == r1 {
			start = c1
		}
		if row == r2 {
			end = c2
		}
		lines[row] = highlightSpan(plain, start, end)
	}
	return strings.Join(lines, "\n")
}

// highlightSpan paints visual columns [start, end) of a plain line in the
// selection style, leaving the rest plain.
func highlightSpan(plain string, start, end int) string {
	w := ansi.StringWidth(plain)
	start = min(max(start, 0), w)
	end = min(max(end, 0), w)
	if start >= end {
		return plain
	}
	return cutCols(plain, 0, start) + selHighlightStyle.Render(cutCols(plain, start, end)) + cutCols(plain, end, w)
}

// cutCols returns the substring of plain text s covering visual columns
// [from, to). Columns are terminal cells, so wide runes (CJK, emoji) count as
// two; a rune is kept with the side holding its starting column.
func cutCols(s string, from, to int) string {
	if from < 0 {
		from = 0
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if col >= to {
			break
		}
		if col >= from {
			b.WriteRune(r)
		}
		col += runewidth.RuneWidth(r)
	}
	return b.String()
}
