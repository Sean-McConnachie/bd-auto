package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The old fixed-column floor was 67 cells. Below it, one logical row became
// two terminal lines, so height windowing counted the wrong thing and could
// hide the selection. Header and rows are clipped as whole rendered lines.
func TestTheTableDoesNotWrapBelowTheFormerMinimumWidth(t *testing.T) {
	for _, width := range []int{64, 40, 12} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newTestModel(newPressed())
			m.Width, m.Height = width, 14
			ids := scopeOf(t, m, 18)
			for i := 0; i < len(ids)-1; i++ {
				m.move(1)
			}

			header := clip(m.header(), width)
			if got := lipgloss.Width(header); got > width {
				t.Fatalf("header width = %d, terminal width = %d: %q", got, width, header)
			}
			selected := m.line(m.Row(ids[len(ids)-1]), true, m.now())
			plain := m.line(m.Row(ids[len(ids)-1]), false, m.now())
			if got := lipgloss.Width(selected); got > width {
				t.Fatalf("selected row width = %d, terminal width = %d: %q", got, width, selected)
			}
			if selected == plain || !strings.Contains(selected, ">") {
				t.Fatalf("selected-row rendering was lost: selected %q, plain %q", selected, plain)
			}

			view := m.View()
			if got := lipgloss.Height(view); got > m.Height {
				t.Fatalf("view height = %d, terminal height = %d:\n%s", got, m.Height, view)
			}
			if !strings.Contains(view, ids[len(ids)-1]) && width >= len(ids[len(ids)-1])+2 {
				t.Fatalf("selected row %s is outside the narrow window:\n%s", ids[len(ids)-1], view)
			}
			body, _ := m.tableBody(m.now())
			if len(body) != len(ids) {
				t.Fatalf("%d logical rows rendered as %d table lines", len(ids), len(body))
			}
			for _, line := range body {
				if got := lipgloss.Width(line); got > width {
					t.Fatalf("table line width = %d, terminal width = %d: %q", got, width, line)
				}
			}
		})
	}
}
