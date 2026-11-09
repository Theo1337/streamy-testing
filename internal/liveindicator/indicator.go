// Package liveindicator draws the small blinking red dot that reminds the
// streamer they are live.
//
// The dot is a layered, always-on-top, click-through window. It is excluded
// from screen capture, so it is a cue for the person streaming rather than
// something the viewers see. On platforms other than Windows the package is a
// no-op.
package liveindicator

// Positions accepted by Show. They intentionally mirror the config.Indicator*
// constants, but liveindicator keeps its own copy so the overlay does not
// depend on the settings package.
const (
	TopLeft     = "top-left"
	TopRight    = "top-right"
	BottomLeft  = "bottom-left"
	BottomRight = "bottom-right"
)

// Controller is the platform independent API the app uses.
type Controller interface {
	// Show makes the dot visible in the requested corner. Calling Show while
	// it is already visible just moves it.
	Show(position string)
	// Hide removes the dot from the screen.
	Hide()
	// SetViewerCount updates the viewer count displayed next to the dot.
	// A count of 0 or less hides the number.
	SetViewerCount(count int)
	// SetShowViewerCount enables or disables the viewer count display.
	SetShowViewerCount(enabled bool)
	// Close destroys the native window and releases its resources.
	Close()
}

// normalizePosition maps anything unexpected to the default corner, so a
// future config value cannot make the dot disappear entirely.
func normalizePosition(position string) string {
	switch position {
	case TopLeft, BottomLeft, BottomRight:
		return position
	default:
		return TopRight
	}
}
