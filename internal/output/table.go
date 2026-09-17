package output

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// DefaultWidth is the assumed terminal width when none was detected. 100 is
// wide enough for id + title + slug + status, which is what `pay find` prints.
const DefaultWidth = 100

// minColumnWidth keeps a truncated column from collapsing to nothing.
const minColumnWidth = 6

// columnGap is the number of spaces between columns.
const columnGap = 2

// renderTable writes an aligned, width-truncated table (§10.3). It is for
// humans; the help text never recommends it to an agent, because truncation
// makes it lossy by design.
func renderTable(w io.Writer, env *Envelope, columns []string, width int) error {
	if width <= 0 {
		width = DefaultWidth
	}
	docs := docsOf(env.Data)
	rows := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		if m, ok := d.(map[string]any); ok {
			rows = append(rows, m)
			continue
		}
		rows = append(rows, map[string]any{"value": d})
	}
	if len(rows) == 0 {
		_, err := io.WriteString(w, "(no rows)\n")
		return err
	}

	cols := columns
	if len(cols) == 0 {
		cols = defaultColumns(rows)
	}
	cells := make([][]string, 0, len(rows))
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c)
	}
	for _, row := range rows {
		flat := flatten(row, "")
		record := make([]string, len(cols))
		for i, c := range cols {
			s := singleLine(cellString(lookup(row, flat, c)))
			record[i] = s
			if n := utf8.RuneCountInString(s); n > widths[i] {
				widths[i] = n
			}
		}
		cells = append(cells, record)
	}
	widths = fitWidths(widths, width)

	if err := writeRow(w, upperAll(cols), widths); err != nil {
		return err
	}
	for _, record := range cells {
		if err := writeRow(w, record, widths); err != nil {
			return err
		}
	}
	return nil
}

// fitWidths shrinks the widest columns until the whole row fits the terminal.
// Shrinking the widest first keeps narrow columns such as id and _status
// intact, which is what makes the table readable at all.
func fitWidths(widths []int, total int) []int {
	out := append([]int(nil), widths...)
	budget := total - columnGap*(len(out)-1)
	if budget < len(out)*minColumnWidth {
		budget = len(out) * minColumnWidth
	}
	for sum(out) > budget {
		widest := 0
		for i, w := range out {
			if w > out[widest] {
				widest = i
			}
		}
		if out[widest] <= minColumnWidth {
			break
		}
		out[widest]--
	}
	return out
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

func writeRow(w io.Writer, record []string, widths []int) error {
	var b strings.Builder
	for i, cell := range record {
		s := truncate(cell, widths[i])
		b.WriteString(s)
		if i < len(record)-1 {
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(s)+columnGap))
		}
	}
	_, err := fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	return err
}

// truncate cuts to n runes, marking the cut with an ellipsis so a reader can
// never mistake a truncated value for a complete one.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// singleLine collapses newlines and tabs so one rich value cannot break the
// alignment of every row below it.
func singleLine(s string) string {
	r := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(r.Replace(s))
}

func upperAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToUpper(s)
	}
	return out
}
