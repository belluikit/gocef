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
	Dir           string
	HelperPath    string
	CachePath     string
	RootCachePath string
	Switches      []string
	LogFile       string
	SandboxPath   string
	PumpScheduler func(delayMS int64)
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
	ScreenScale() float32
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

// ScreenScale defaults to an unscaled display. Unlike ViewSize there is a safe
// default here, so NopHandler can supply it.
func (NopHandler) ScreenScale() float32 { return 1 }

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

// Stats reports how many times each shim callback has fired.
type Stats struct {
	ViewRect     int64
	ScreenInfo   int64
	Paint        int64
	AfterCreated int64
	BeforeClose  int64
}

// ReadStats reports zeroes on a target with no binding.
func ReadStats() Stats { return Stats{} }

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

// MouseButton identifies a button for SendMouseClick.
type MouseButton int

const (
	MouseLeft MouseButton = iota
	MouseMiddle
	MouseRight
)

// KeyEventType identifies a key event.
type KeyEventType int

const (
	KeyRawDown KeyEventType = iota
	KeyDown
	KeyUp
	KeyChar
)

// DevToolsHandler is an optional capability on a Handler: implement it to
// receive DevTools-protocol traffic.
type DevToolsHandler interface {
	DevToolsMessage(msg []byte)
}

// ErrAborted is the Chromium net error code of a cancelled navigation.
const ErrAborted = -3

// LoadHandler is an optional capability on a Handler: implement it to observe
// the browser's load progress.
type LoadHandler interface {
	LoadingStateChanged(loading bool, url string)
	LoadError(mainFrame bool, code int, errorText, url string)
}

// NavigationEvent is a navigation the browser is about to start.
type NavigationEvent struct {
	URL         string
	Transition  uint32
	MainFrame   bool
	UserGesture bool
	Redirect    bool
}

// Transition source values and flags for NavigationEvent.Transition.
const (
	TransitionLink            = 0
	TransitionExplicit        = 1
	TransitionAutoSubframe    = 3
	TransitionManualSubframe  = 4
	TransitionFormSubmit      = 7
	TransitionReload          = 8
	TransitionSourceMask      = 0xFF
	TransitionForwardBackFlag = 0x01000000
)

// NavigationHandler is an optional capability on a Handler: implement it to
// decide navigations before they start.
type NavigationHandler interface {
	BeforeBrowse(ev NavigationEvent) bool
	BeforePopup(url string, userGesture bool)
}

// Range is a range in UTF-16 code units.
type Range struct {
	From, To int
}

// Rect is a rectangle in view coordinates (DIP).
type Rect struct {
	X, Y, Width, Height int
}

// CompositionUnderline styles one segment of the composition text.
type CompositionUnderline struct {
	Range Range
	Thick bool
}

// ImeHandler is an optional capability on a Handler: implement it to receive
// composition-range reports while an IME composition is active.
type ImeHandler interface {
	ImeCompositionRangeChanged(sel Range, bounds []Rect)
}

func (b *Browser) SendMouseMove(int, int, uint32, bool)                         {}
func (b *Browser) SendMouseClick(int, int, uint32, MouseButton, bool, int)      {}
func (b *Browser) SendMouseWheel(int, int, uint32, int, int)                    {}
func (b *Browser) SendKey(KeyEventType, uint32, int, int, uint16, uint16, bool) {}
func (b *Browser) ImeSetComposition(string, int, []CompositionUnderline)        {}
func (b *Browser) ImeCommitText(string)                                         {}
func (b *Browser) ImeFinishComposingText(bool)                                  {}
func (b *Browser) ImeCancelComposition()                                        {}
func (b *Browser) CanGoBack() bool                                              { return false }
func (b *Browser) CanGoForward() bool                                           { return false }
func (b *Browser) GoBack()                                                      {}
func (b *Browser) GoForward()                                                   {}
func (b *Browser) Reload(bool)                                                  {}
func (b *Browser) StopLoad()                                                    {}
func (b *Browser) SendDevTools([]byte) bool                                     { return false }
