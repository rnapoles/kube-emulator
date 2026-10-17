package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rnapoles/kube-emulator/internal/trace"
)

// Options configures the terminal UI.
type Options struct {
	Rec  *trace.Recorder
	Logs *LogBuffer
	URL  string // the address the emulator listens on, shown in the header
}

// Run shows the UI until the user quits (q or ctrl+c) or ctx is cancelled.
func Run(ctx context.Context, opt Options) error {
	p := tea.NewProgram(newModel(opt), tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		<-ctx.Done()
		p.Quit()
	}()
	_, err := p.Run()
	return err
}
