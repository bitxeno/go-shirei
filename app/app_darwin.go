//go:build darwin && !ios

package app

import (
	"image"

	"go.hasen.dev/shirei"
	"go.hasen.dev/shirei/cocoabackend"
	"go.hasen.dev/shirei/internal/iconimg"
)

// SetupWindow records the window's title and initial size in points. Call it
// before Run.
func SetupWindow(title string, width, height int) {
	cocoabackend.SetupWindow(title, width, height)
}

// SetupPopupMode switches the window to a non-activating popup panel
// (clipboard-manager style): it never steals input focus, starts hidden, and
// hides itself on focus loss. Call it before Run. macOS only.
func SetupPopupMode() {
	cocoabackend.SetupPopupMode()
}

// TogglePopup shows the window at the mouse cursor or hides it. Panel mode
// only; no-op before Run.
func TogglePopup() {
	cocoabackend.TogglePopup()
}

// HidePopup hides the window when visible. Panel mode only.
func HidePopup() {
	cocoabackend.HidePopup()
}

// SetupIcon records the path of the image (PNG etc.) used as the app's icon —
// shown wherever the platform shows one (macOS: Dock; Windows: title bar and
// taskbar; X11: wherever the WM displays _NET_WM_ICON; Wayland: via
// xdg-toplevel-icon-v1 where the compositor ships it, otherwise a .desktop
// file matched by app_id). Optional; call it before Run.
func SetupIcon(imagePath string) {
	cocoabackend.SetupIcon(imagePath)
}

// SetupIconImage is SetupIcon from an in-memory image instead of a file. It
// takes precedence over SetupIcon. Optional; call it before Run.
func SetupIconImage(img image.Image) {
	cocoabackend.SetupIconImage(img)
}

// SetupIconBytes is SetupIcon from encoded image bytes (PNG etc.), e.g. a
// go:embed-ed asset. It takes precedence over SetupIcon; bytes that fail to
// decode leave the default icon. Optional; call it before Run.
func SetupIconBytes(data []byte) {
	if img := iconimg.DecodeBytes(data); img != nil {
		SetupIconImage(img)
	}
}

// Run opens the window and runs the native event loop, invoking frameFn once per
// frame. It must be called from the program's main goroutine and does not return
// until the app exits.
func Run(frameFn shirei.FrameFn) {
	cocoabackend.Run(frameFn)
}
