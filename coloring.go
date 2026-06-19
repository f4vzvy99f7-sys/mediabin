package main

import (
	"fmt"
	"golang.org/x/term"
	"os"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorGray   = "\033[90m"
)

// isTTY reports whether stdout is an interactive terminal.
func isTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// formatBytes formats a byte count into a human-readable string.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	labels := []string{"K", "M", "G", "T"}
	return fmt.Sprintf("%.2f %sB", float64(n)/float64(div), labels[exp])
}
