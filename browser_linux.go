//go:build linux && cgo

package gocef

/*
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Handler receives everything a windowless browser reports. Every method is
// called on CEF's browser UI thread — the thread that calls Work — so a
// consumer that already runs its UI there needs no locking.
//
// All methods are optional in spirit but required by the interface; embed
// NopHandler to implement only what you need.
type Handler interface {
	// ViewSize is asked for the view's current size in pixels. CEF requires a
	// non-empty rectangle, so it must always answer.
	ViewSize() (w, h int)

	// ScreenScale is asked for the display's device scale factor — the ratio
	// between physical and logical pixels. 1 for a conventional display, 2 for
	// a typical HiDPI one, and fractional values are normal on Linux.
	//
	// CEF's compositor decides its raster resolution from this, so a host that
	// answers 1 on a scaled display gets content rasterised at 1x and scaled
	// up: soft text and blurred hairlines that nothing downstream can repair.
	//
	// It is asked per call rather than captured once, because a window can move
	// between monitors of different scales.
	ScreenScale() float32

	// Paint delivers a CPU frame: BGRA, upper-left origin. The buffer is valid
	// only for the duration of the call — copy what you keep.
	//
	// popup reports the separate surface CEF uses for <select> dropdowns, which
	// must be composited over the view. Ignoring it renders dropdowns invisible.
	Paint(buf []byte, w, h int, popup bool)

	// AcceleratedPaint delivers a dma-buf frame when shared textures are
	// enabled. The fd is valid ONLY inside this call: CEF documents that the
	// resource "cannot be cached and cannot be accessed outside of this
	// callback" and that its contents "should be copied to a texture owned by
	// the client application". So an importer has to do its work synchronously
	// here — this is not a zero-copy handoff (belluikit/bellui#113).
	AcceleratedPaint(f AcceleratedFrame)

	// PopupShow and PopupRect track the dropdown surface's visibility and
	// placement.
	PopupShow(show bool)
	PopupRect(x, y, w, h int)

	// CursorChange reports a requested cursor as a cef_cursor_type_t value.
	CursorChange(cursor int)

	// Created and Closed bracket the browser's life.
	Created()
	Closed()
}

// AcceleratedFrame describes a dma-buf frame. Observed on NVIDIA under X11:
// one plane, BGRA_8888, DRM_FORMAT_MOD_LINEAR.
type AcceleratedFrame struct {
	PlaneCount int
	FD         int
	Stride     uint32
	Modifier   uint64
	Format     int
}

// NopHandler implements Handler with do-nothing methods, apart from ViewSize,
// which must be supplied — there is no safe default for it.
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

// Browser is one windowless browser.
type Browser struct {
	id      uint64
	handler Handler
	closed  bool
}

// NewBrowser creates a windowless browser showing url at w*h pixels.
//
// sharedTexture asks for dma-buf frames through Handler.AcceleratedPaint
// instead of CPU buffers through Paint. It additionally needs a live display
// session and Chromium's native Vulkan renderer; without both, CEF falls back
// to Paint (belluikit/bellui#102).
func NewBrowser(url string, w, h int, sharedTexture bool, handler Handler) (*Browser, error) {
	if handler == nil {
		return nil, errors.New("gocef: handler is required")
	}

	mu.Lock()
	if !started {
		mu.Unlock()
		return nil, ErrNotLoaded
	}
	nextID++
	b := &Browser{id: nextID, handler: handler}
	browsers[b.id] = b
	mu.Unlock()

	curl := C.CString(url)
	defer C.free(unsafe.Pointer(curl))

	shared := C.int(0)
	if sharedTexture {
		shared = 1
	}
	if C.gocef_create_browser(C.uint64_t(b.id), curl, C.int(w), C.int(h), shared) != 0 {
		mu.Lock()
		delete(browsers, b.id)
		mu.Unlock()
		return nil, fmt.Errorf("gocef: %s", C.GoString(C.gocef_error()))
	}
	return b, nil
}

// Resize tells CEF the view changed size. CEF re-reads the size through
// Handler.ViewSize, so update whatever that reads before calling this.
func (b *Browser) Resize(w, h int) {
	C.gocef_resize(C.uint64_t(b.id), C.int(w), C.int(h))
}

// SetVisible drives visibility. A windowless browser has no window whose
// visibility CEF can observe, so it must be told — drive this from show/hide
// and from scrolling out of a clipping ancestor.
func (b *Browser) SetVisible(visible bool) {
	C.gocef_set_visible(C.uint64_t(b.id), cbool(visible))
}

// SetFocus drives keyboard focus into or out of the page.
func (b *Browser) SetFocus(focused bool) {
	C.gocef_set_focus(C.uint64_t(b.id), cbool(focused))
}

// Invalidate asks for a repaint of the whole view.
func (b *Browser) Invalidate() {
	C.gocef_invalidate(C.uint64_t(b.id))
}

// LoadURL navigates the main frame.
func (b *Browser) LoadURL(url string) {
	curl := C.CString(url)
	defer C.free(unsafe.Pointer(curl))
	C.gocef_load_url(C.uint64_t(b.id), curl)
}

// Close closes the browser. Handler.Closed fires when CEF has finished.
func (b *Browser) Close() {
	if b.closed {
		return
	}
	b.closed = true
	C.gocef_close_browser(C.uint64_t(b.id))
}

func cbool(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

// lookup resolves a callback's handle. A callback can arrive for a browser the
// Go side has already forgotten — teardown is not instantaneous — so a miss is
// normal and must not panic.
func lookup(handle C.uint64_t) *Browser {
	mu.Lock()
	defer mu.Unlock()
	return browsers[uint64(handle)]
}

//export gocefGetViewRect
func gocefGetViewRect(handle C.uint64_t, w, h *C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	bw, bh := b.handler.ViewSize()
	*w = C.int(bw)
	*h = C.int(bh)
}

//export gocefGetScreenScale
func gocefGetScreenScale(handle C.uint64_t) C.float {
	b := lookup(handle)
	if b == nil {
		return 1
	}
	s := b.handler.ScreenScale()
	if s <= 0 {
		// CEF divides by this; a zero would be a compositor fault reported far
		// from the handler that produced it.
		return 1
	}
	return C.float(s)
}

//export gocefOnPaint
func gocefOnPaint(handle C.uint64_t, buffer unsafe.Pointer, w, h C.int, isPopup C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	n := int(w) * int(h) * 4
	// Borrowed for the duration of the call only; the handler copies what it
	// keeps. This is why Paint documents the lifetime.
	buf := unsafe.Slice((*byte)(buffer), n)
	b.handler.Paint(buf, int(w), int(h), isPopup != 0)
}

//export gocefOnAcceleratedPaint
func gocefOnAcceleratedPaint(handle C.uint64_t, planeCount, fd C.int, stride C.uint32_t, modifier C.uint64_t, format C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	b.handler.AcceleratedPaint(AcceleratedFrame{
		PlaneCount: int(planeCount),
		FD:         int(fd),
		Stride:     uint32(stride),
		Modifier:   uint64(modifier),
		Format:     int(format),
	})
}

//export gocefOnPopupShow
func gocefOnPopupShow(handle C.uint64_t, show C.int) {
	if b := lookup(handle); b != nil {
		b.handler.PopupShow(show != 0)
	}
}

//export gocefOnPopupSize
func gocefOnPopupSize(handle C.uint64_t, x, y, w, h C.int) {
	if b := lookup(handle); b != nil {
		b.handler.PopupRect(int(x), int(y), int(w), int(h))
	}
}

//export gocefOnCursorChange
func gocefOnCursorChange(handle C.uint64_t, cursorType C.int) {
	if b := lookup(handle); b != nil {
		b.handler.CursorChange(int(cursorType))
	}
}

//export gocefOnAfterCreated
func gocefOnAfterCreated(handle C.uint64_t) {
	if b := lookup(handle); b != nil {
		b.handler.Created()
	}
}

//export gocefOnBeforeClose
func gocefOnBeforeClose(handle C.uint64_t) {
	b := lookup(handle)
	if b == nil {
		return
	}
	b.handler.Closed()
	mu.Lock()
	delete(browsers, uint64(handle))
	mu.Unlock()
}

// MouseButton identifies a button for SendMouseClick, matching
// cef_mouse_button_type_t.
type MouseButton int

const (
	MouseLeft MouseButton = iota
	MouseMiddle
	MouseRight
)

// KeyEventType identifies a key event, matching cef_key_event_type_t.
//
// A typed character normally needs two events: a KeyDown carrying the key code,
// then a KeyChar carrying the rune. Sending only one of them is the usual reason
// a page sees modifiers but no text, or text with no keydown handler firing.
type KeyEventType int

const (
	KeyRawDown KeyEventType = iota
	KeyDown
	KeyUp
	KeyChar
)

// SendMouseMove sends a pointer move. leave reports the pointer leaving the
// view, which is what lets a page clear its hover state.
func (b *Browser) SendMouseMove(x, y int, modifiers uint32, leave bool) {
	C.gocef_send_mouse_move(C.uint64_t(b.id), C.int(x), C.int(y),
		C.uint32_t(modifiers), cbool(leave))
}

// SendMouseClick sends a press or release. clickCount carries double- and
// triple-click, which is how a page distinguishes word and line selection.
func (b *Browser) SendMouseClick(x, y int, modifiers uint32, button MouseButton, up bool, clickCount int) {
	C.gocef_send_mouse_click(C.uint64_t(b.id), C.int(x), C.int(y),
		C.uint32_t(modifiers), C.int(button), cbool(up), C.int(clickCount))
}

// SendMouseWheel scrolls at a point. Deltas are in pixels.
func (b *Browser) SendMouseWheel(x, y int, modifiers uint32, deltaX, deltaY int) {
	C.gocef_send_mouse_wheel(C.uint64_t(b.id), C.int(x), C.int(y),
		C.uint32_t(modifiers), C.int(deltaX), C.int(deltaY))
}

// SendKey sends one key event. See KeyEventType for why text usually needs two.
func (b *Browser) SendKey(t KeyEventType, modifiers uint32, windowsKeyCode, nativeKeyCode int, char, unmodifiedChar uint16, systemKey bool) {
	C.gocef_send_key(C.uint64_t(b.id), C.int(t), C.uint32_t(modifiers),
		C.int(windowsKeyCode), C.int(nativeKeyCode), C.uint16_t(char),
		C.uint16_t(unmodifiedChar), cbool(systemKey))
}

// CanGoBack reports whether there is history to go back to.
func (b *Browser) CanGoBack() bool {
	return C.gocef_nav(C.uint64_t(b.id), C.GOCEF_NAV_CAN_GO_BACK) != 0
}

// CanGoForward reports whether there is history to go forward to.
func (b *Browser) CanGoForward() bool {
	return C.gocef_nav(C.uint64_t(b.id), C.GOCEF_NAV_CAN_GO_FORWARD) != 0
}

// GoBack navigates back.
func (b *Browser) GoBack() { C.gocef_nav(C.uint64_t(b.id), C.GOCEF_NAV_GO_BACK) }

// GoForward navigates forward.
func (b *Browser) GoForward() { C.gocef_nav(C.uint64_t(b.id), C.GOCEF_NAV_GO_FORWARD) }

// Reload reloads the page. ignoreCache bypasses the HTTP cache.
func (b *Browser) Reload(ignoreCache bool) {
	op := C.int(C.GOCEF_NAV_RELOAD)
	if ignoreCache {
		op = C.GOCEF_NAV_RELOAD_NO_CACHE
	}
	C.gocef_nav(C.uint64_t(b.id), op)
}

// StopLoad stops the current navigation.
func (b *Browser) StopLoad() { C.gocef_nav(C.uint64_t(b.id), C.GOCEF_NAV_STOP) }

// SendDevTools sends one DevTools-protocol message — raw JSON, exactly as the
// protocol defines it — and reports whether CEF accepted it.
//
// This is the whole bridge surface. Script injection, a page-to-host message
// channel and eval-with-result are all reachable from here
// (Page.addScriptToEvaluateOnNewDocument, Runtime.addBinding,
// Runtime.evaluate), from the browser process, with no renderer-side code —
// which is why bellui's backend can use it instead of a CefV8Handler.
//
// Replies and events arrive on DevToolsHandler.DevToolsMessage.
func (b *Browser) SendDevTools(msg []byte) bool {
	if len(msg) == 0 {
		return false
	}
	return C.gocef_send_devtools(C.uint64_t(b.id), unsafe.Pointer(&msg[0]), C.size_t(len(msg))) != 0
}

// DevToolsHandler is an optional capability on a Handler: implement it to
// receive DevTools-protocol traffic. A Handler that does not simply never sees
// it, and no observer is registered for it.
type DevToolsHandler interface {
	// DevToolsMessage delivers raw protocol JSON — a reply to a SendDevTools
	// call, or an event. The buffer is valid only for the duration of the call.
	DevToolsMessage(msg []byte)
}

// ErrAborted is the Chromium net error code of a cancelled navigation — one
// refused by NavigationHandler.BeforeBrowse, stopped with StopLoad, or
// replaced by a newer navigation. It reaches LoadHandler.LoadError like any
// other failure, so a consumer that reports its own cancellations filters it
// there. The value is net::ERR_ABORTED from Chromium's net_error_list.h; CEF
// does not re-declare it in its headers.
const ErrAborted = -3

// LoadHandler is an optional capability on a Handler: implement it to observe
// the browser's load progress. A Handler that does not is simply never told.
type LoadHandler interface {
	// LoadingStateChanged fires exactly twice per navigation: once when
	// loading begins and once when it settles — by completing, failing or
	// being cancelled. It fires before any LoadError of the same navigation's
	// end. url is the main frame's URL at that moment.
	LoadingStateChanged(loading bool, url string)

	// LoadError reports a navigation that failed or was cancelled. code is a
	// Chromium net error (see ErrAborted for the one worth special-casing),
	// errorText its short name, and url the address that did not load.
	LoadError(mainFrame bool, code int, errorText, url string)
}

// NavigationEvent is a navigation the browser is about to start, offered to
// NavigationHandler.BeforeBrowse.
type NavigationEvent struct {
	// URL is where the browser is about to go.
	URL string

	// Transition says what started the navigation, as a
	// cef_transition_type_t value: mask with TransitionSourceMask for the
	// source, test TransitionForwardBackFlag for history moves.
	Transition uint32

	// MainFrame reports the view's own navigation rather than a sub-frame's.
	MainFrame bool

	// UserGesture reports an explicit user action (e.g. clicking a link)
	// rather than an automatic navigation.
	UserGesture bool

	// Redirect reports a server- or script-initiated redirect.
	Redirect bool
}

// Transition source values and flags for NavigationEvent.Transition, matching
// cef_transition_type_t.
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
// decide navigations before they start. Without it every navigation proceeds
// and popup requests are dropped.
type NavigationHandler interface {
	// BeforeBrowse is consulted before each navigation. The engine is blocked
	// on the answer, so it must return promptly. Return false to cancel; a
	// cancelled navigation additionally reports LoadError with ErrAborted.
	BeforeBrowse(ev NavigationEvent) bool

	// BeforePopup reports the page asking to open url in a new window —
	// target="_blank" or window.open. The popup itself is always refused: a
	// windowless browser has nowhere to host a second window. The handler
	// decides what becomes of the URL, typically by loading it in this
	// browser or ignoring it.
	BeforePopup(url string, userGesture bool)
}

// goStringN copies a shim-owned UTF-8 buffer that may be NULL.
func goStringN(p unsafe.Pointer, n C.int) string {
	if p == nil || n <= 0 {
		return ""
	}
	return string(unsafe.Slice((*byte)(p), int(n)))
}

//export gocefOnLoadingStateChange
func gocefOnLoadingStateChange(handle C.uint64_t, isLoading C.int, url unsafe.Pointer, urlLen C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	if h, ok := b.handler.(LoadHandler); ok {
		h.LoadingStateChanged(isLoading != 0, goStringN(url, urlLen))
	}
}

//export gocefOnLoadError
func gocefOnLoadError(handle C.uint64_t, isMainFrame, errorCode C.int, errorText unsafe.Pointer, errorTextLen C.int, url unsafe.Pointer, urlLen C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	if h, ok := b.handler.(LoadHandler); ok {
		h.LoadError(isMainFrame != 0, int(errorCode),
			goStringN(errorText, errorTextLen), goStringN(url, urlLen))
	}
}

//export gocefOnBeforeBrowse
func gocefOnBeforeBrowse(handle C.uint64_t, url unsafe.Pointer, urlLen C.int, transition C.uint32_t, isMainFrame, userGesture, isRedirect C.int) C.int {
	b := lookup(handle)
	if b == nil {
		return 1 // a browser Go has forgotten has no policy left to consult
	}
	h, ok := b.handler.(NavigationHandler)
	if !ok {
		return 1
	}
	allowed := h.BeforeBrowse(NavigationEvent{
		URL:         goStringN(url, urlLen),
		Transition:  uint32(transition),
		MainFrame:   isMainFrame != 0,
		UserGesture: userGesture != 0,
		Redirect:    isRedirect != 0,
	})
	return cbool(allowed)
}

//export gocefOnBeforePopup
func gocefOnBeforePopup(handle C.uint64_t, url unsafe.Pointer, urlLen C.int, userGesture C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	if h, ok := b.handler.(NavigationHandler); ok {
		h.BeforePopup(goStringN(url, urlLen), userGesture != 0)
	}
}

//export gocefOnDevToolsMessage
func gocefOnDevToolsMessage(handle C.uint64_t, msg unsafe.Pointer, n C.int) {
	b := lookup(handle)
	if b == nil {
		return
	}
	h, ok := b.handler.(DevToolsHandler)
	if !ok {
		return
	}
	h.DevToolsMessage(unsafe.Slice((*byte)(msg), int(n)))
}
