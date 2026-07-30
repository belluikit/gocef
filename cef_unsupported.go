//go:build !linux || !cgo

package gocef

import "errors"

// Supported reports whether this build has a CEF binding.
//
// CEF is bound on linux with cgo enabled. Everything else compiles this file,
// so a cross-platform program builds everywhere and fails at Load rather than
// at link time — the same bargain the bellui webview package strikes for
// platforms with no web engine.
const Supported = false

// ErrRuntimeMissing is reported when no CEF payload can be found or loaded. It
// is declared on every target so a caller can test for it with errors.Is
// without a build tag.
var ErrRuntimeMissing = errors.New("gocef: no CEF payload found")

// ErrNotLoaded is reported by calls made before a successful Load.
var ErrNotLoaded = errors.New("gocef: library not loaded")

// ErrUnsupported is reported by every entry point on a target with no binding.
var ErrUnsupported = errors.New("gocef: unsupported platform (needs linux and cgo)")

// Config describes the payload layout and the Chromium command line. It is
// declared here so cross-platform code can build one unconditionally.
type Config struct {
	Dir        string
	HelperPath string
	CachePath  string
	Switches   []string
}

// AcceleratedFrame describes a dma-buf frame.
type AcceleratedFrame struct {
	PlaneCount int
	FD         int
	Stride     uint32
	Modifier   uint64
	Format     int
}

// Handler receives everything a windowless browser reports.
type Handler interface {
	ViewSize() (w, h int)
	Paint(buf []byte, w, h int, popup bool)
	AcceleratedPaint(f AcceleratedFrame)
	PopupShow(show bool)
	PopupRect(x, y, w, h int)
	CursorChange(cursor int)
	Created()
	Closed()
}

// NopHandler implements Handler with do-nothing methods, apart from ViewSize.
type NopHandler struct{}

func (NopHandler) Paint([]byte, int, int, bool)      {}
func (NopHandler) AcceleratedPaint(AcceleratedFrame) {}
func (NopHandler) PopupShow(bool)                    {}
func (NopHandler) PopupRect(int, int, int, int)      {}
func (NopHandler) CursorChange(int)                  {}
func (NopHandler) Created()                          {}
func (NopHandler) Closed()                           {}

// Browser is one windowless browser. It cannot be created on this target.
type Browser struct{}

// Locate reports that there is no payload to find on this target.
func Locate() (string, error) { return "", ErrUnsupported }

// Load reports that there is nothing to load on this target.
func Load(string) error { return ErrUnsupported }

// Initialize reports that CEF cannot start on this target.
func Initialize(Config) error { return ErrUnsupported }

// Work is a no-op without a browser process.
func Work() {}

// Shutdown is a no-op without a browser process.
func Shutdown() {}

// ExecuteProcess reports the browser-process sentinel, since no subprocess can
// be recognised here.
func ExecuteProcess([]string) int { return -1 }

// NewBrowser reports that no browser can be created on this target.
func NewBrowser(string, int, int, bool, Handler) (*Browser, error) {
	return nil, ErrUnsupported
}

func (b *Browser) Resize(int, int) {}
func (b *Browser) SetVisible(bool) {}
func (b *Browser) SetFocus(bool)   {}
func (b *Browser) Invalidate()     {}
func (b *Browser) LoadURL(string)  {}
func (b *Browser) Close()          {}
