package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// isTerminal returns true if the given file descriptor appears to be a terminal.
// We use a simple heuristic: if stdout is a terminal, colors are enabled.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// colorsEnabled returns true when stdout is a real terminal (not piped/redirected).
var colorsEnabled = isTerminal(os.Stdout)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
)

func colored(color, text string) string {
	if !colorsEnabled {
		return text
	}
	return color + text + colorReset
}

// printJSON marshals v as indented JSON and writes it to w.
// Returns an error if v cannot be marshaled.
func printJSON(w io.Writer, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("printJSON: marshal: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// printTable writes a simple aligned table to w.
// Headers are printed first, followed by a separator, then each row.
func printTable(w io.Writer, headers []string, rows [][]string) {
	// Calculate column widths
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Print header
	for i, h := range headers {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprintf(w, "%-*s", widths[i], h)
	}
	fmt.Fprintln(w)

	// Print separator
	for i, w2 := range widths {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprint(w, strings.Repeat("-", w2))
	}
	fmt.Fprintln(w)

	// Print rows
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				if i > 0 {
					fmt.Fprint(w, "  ")
				}
				fmt.Fprintf(w, "%-*s", widths[i], cell)
			}
		}
		fmt.Fprintln(w)
	}
}

// printSuccess writes a success message (green checkmark + message) to w.
func printSuccess(w io.Writer, msg string) {
	fmt.Fprintln(w, colored(colorGreen, "✓")+" "+msg)
}

// printError writes an error message (red X + message) to w.
func printError(w io.Writer, msg string) {
	fmt.Fprintln(w, colored(colorRed, "✗")+" "+msg)
}

// printWarning writes a warning message (yellow ! + message) to w.
func printWarning(w io.Writer, msg string) {
	fmt.Fprintln(w, colored(colorYellow, "!")+" "+msg)
}
