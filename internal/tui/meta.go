package tui

import (
	"fmt"
	"strings"
	"time"
)

func handleMeta(m *model, line string) bool {
	cmd := strings.TrimSpace(line)
	switch cmd {
	case ":q", ":quit", ":exit":
		m.quitting = true
		return true
	case ":clear":
		m.outputs = nil
		m.scroll = 0
		m.editor.clear()
		m.setMessage("output cleared")
		return true
	case ":layout":
		m.addOutput(outputBlock{
			Command:  ":layout",
			Stdout:   fmt.Sprintf("terminal %dx%d; ticker=%v warning=%v", m.size.Cols, m.size.Rows, computeLayout(m.size).showTicker, computeLayout(m.size).showWarning),
			ExitCode: 0,
			Started:  time.Now(),
			Finished: time.Now(),
		})
		return true
	case ":help":
		m.addOutput(outputBlock{
			Command:  ":help",
			Stdout:   tuiHelp(m),
			ExitCode: 0,
			Started:  time.Now(),
			Finished: time.Now(),
		})
		return true
	}
	return false
}

func tuiHelp(m *model) string {
	var b strings.Builder
	b.WriteString("TUI commands:\n")
	b.WriteString("  :help    show this help\n")
	b.WriteString("  :clear   clear output scrollback\n")
	b.WriteString("  :layout  show current responsive layout\n")
	b.WriteString("  :quit    exit\n\n")
	b.WriteString("Keys:\n")
	b.WriteString("  Tab complete, arrows history, PageUp/PageDown scroll, Ctrl-L clear, Ctrl-C cancel/clear/quit, Ctrl-D/Ctrl-Q quit\n\n")
	b.WriteString("CLI commands:\n")
	for _, spec := range m.catalog {
		b.WriteString("  ")
		b.WriteString(spec.Name)
		if spec.TUI == "external" {
			b.WriteString(" (external)")
		}
		b.WriteString(" - ")
		b.WriteString(spec.Summary)
		b.WriteByte('\n')
	}
	return b.String()
}
