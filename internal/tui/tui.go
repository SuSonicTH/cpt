// Package tui implements the interactive terminal interface.
package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SuSonicTH/cpt/internal/app"
)

// Run starts the interactive UI and blocks until the user quits.
func Run(ctx context.Context, a *app.App) error {
	m := newModel(ctx, a)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	m.shutdown()
	return err
}

// shutdown cancels any running operation and stops the event pump.
func (m *Model) shutdown() {
	if m.quitting {
		return
	}
	m.quitting = true
	if m.ctxCancel != nil {
		m.ctxCancel()
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.probeCancel != nil {
		m.probeCancel()
		m.probeCancel = nil
	}
	if m.unsubscribe != nil {
		m.unsubscribe()
	}
	close(m.done)
}
