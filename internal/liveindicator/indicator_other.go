//go:build !windows

package liveindicator

import "log/slog"

// New returns a controller that does nothing: the overlay is Windows only for
// now. Keeping the API identical means internal/app needs no build tags.
func New(_ *slog.Logger) Controller {
	return noop{}
}

type noop struct{}

func (noop) Show(string)        {}
func (noop) Hide()              {}
func (noop) SetViewerCount(int) {}
func (noop) SetShowViewerCount(bool) {}
func (noop) Close()             {}
