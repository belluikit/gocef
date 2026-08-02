// Shim between Go and CEF's C API.
//
// Go never sees a CEF struct. Everything on the other side of this header is
// plain scalars and opaque handles, so cgo does not have to describe CEF's type
// graph and the Go side stays free of unsafe pointer arithmetic.
//
// Two structural decisions live here, both carried over from the #60 phase-0
// probe (belluikit/bellui#102), which proved them on real hardware:
//
//   - We bind the **C API**. It is exported directly from libcef.so, so
//     libcef_dll_wrapper and the C++ ABI are not involved at all.
//   - libcef.so is resolved with **dlopen at runtime**, never linked. Nothing
//     here needs CEF present to build, so `go build` is hermetic on machines
//     that have never seen a Chromium payload.

#ifndef GOCEF_SHIM_H
#define GOCEF_SHIM_H

#include <stddef.h>
#include <stdint.h>

// ---------------------------------------------------------------------------
// Library and process lifecycle.

// gocef_load dlopens libcef.so from dir, resolves the entry points this shim
// uses, and performs the API-version handshake with apiVersion.
//
// The handshake is not optional and cef_api_hash is what performs it, despite
// reading like a query: its documentation says it "configures the CEF API
// version ... any changes to this value will be ignored after the first call".
// Without it libcef has no configured version, cef_api_version() reports -1,
// and the C-to-C++ layer fatally rejects the first struct it is handed, naming
// whichever type it happened to be validating.
//
// Returns 0 on success. On failure gocef_error describes it.
int gocef_load(const char *dir, int apiVersion);

// gocef_initialize starts the CEF browser process. helperPath is the subprocess
// executable; switches is a space-separated Chromium command line appended to
// argv, which Chromium propagates to the subprocesses it launches.
//
// cachePath is CEF's cache_path — where the profile (cookies, localStorage,
// HTTP cache) persists; empty means incognito. rootCachePath is CEF's
// root_cache_path — installation-specific data and the process-singleton
// lock; empty falls back to cachePath, and with both empty Chromium uses a
// per-user directory shared by every CEF application, which CEF warns
// against.
//
// Must be called from the thread that will pump the message loop.
// logFile, when non-empty, raises CEF's log severity to VERBOSE and writes
// Chromium's own logging there — the diagnostic for a browser that silently
// does nothing.
// enableSandbox selects Chromium's Linux sandbox; the caller must have
// verified a usable SUID helper first (see Config.SandboxPath), because with
// no usable sandbox Chromium aborts the process instead of failing the call.
int gocef_initialize(const char *resourcesDir, const char *localesDir,
                     const char *cachePath, const char *rootCachePath,
                     const char *helperPath, const char *switches,
                     int windowlessFrameRate, const char *logFile,
                     int enableSandbox);

// gocef_work runs one iteration of CEF's message loop. It must be called from
// the same thread that called gocef_initialize — CEF's browser UI thread.
void gocef_work(void);

// gocef_shutdown shuts CEF down. No CEF call may follow it in this process, and
// cef_initialize cannot be called a second time either: initialization is
// once-per-process.
void gocef_shutdown(void);

// gocef_error returns the last failure message, or "" if there was none.
const char *gocef_error(void);

// gocef_counter reports how many times a shim callback has fired, for
// diagnosing a browser that silently does nothing. A stalled creation with
// get_view_rect at zero means CEF never consulted the client at all; nonzero
// counts walk the line to wherever progress stopped.
enum {
  GOCEF_N_GET_VIEW_RECT = 0,
  GOCEF_N_GET_SCREEN_INFO,
  GOCEF_N_PAINT,
  GOCEF_N_AFTER_CREATED,
  GOCEF_N_BEFORE_CLOSE,
  GOCEF_N_COUNTERS,
};
long gocef_counter(int which);

// gocef_compiled_api_version reports the CEF_API_VERSION this shim was built
// with. The Go constant and the -D flag that reaches the compiler are two
// declarations of one fact, and Load asserts they agree — a mismatch would
// otherwise show up as struct sizes that disagree with the handshake, which
// fails far from its cause.
int gocef_compiled_api_version(void);

// gocef_execute_process is the subprocess entry point, for a helper binary that
// links this shim rather than shipping its own copy. Returns the process exit
// code for a recognised subprocess, or -1 for the browser process.
int gocef_execute_process(int argc, char **argv);

// ---------------------------------------------------------------------------
// Browsers.
//
// Each browser is identified by a handle the Go side allocates. Every callback
// carries it, so Go can find the owning object without CEF pointers ever
// reaching Go.

// gocef_create_browser creates a windowless browser. sharedTexture requests
// dma-buf delivery through the accelerated-paint callback instead of CPU
// buffers; it additionally requires a live display session and Chromium's
// native Vulkan renderer (see belluikit/bellui#102).
int gocef_create_browser(uint64_t handle, const char *url, int w, int h,
                         int sharedTexture);

// gocef_close_browser closes a browser and releases the shim state that backs
// its handle.
void gocef_close_browser(uint64_t handle);

// gocef_resize tells CEF the view is now w*h pixels and asks for a repaint.
void gocef_resize(uint64_t handle, int w, int h);

// gocef_set_visible drives CefBrowserHost::WasHidden. A windowless browser has
// no window whose visibility CEF can observe, so it has to be told — this is
// what a host drives from Show/Hide and from scrolling out of a clipping
// ancestor.
void gocef_set_visible(uint64_t handle, int visible);

// gocef_set_focus drives CefBrowserHost::SetFocus.
void gocef_set_focus(uint64_t handle, int focused);

// gocef_invalidate asks for a repaint of the whole view.
void gocef_invalidate(uint64_t handle);

// gocef_load_url navigates the main frame.
void gocef_load_url(uint64_t handle, const char *url);

// Navigation operations, for gocef_nav. Queries report their answer; commands
// report 1 if they were dispatched.
enum {
  GOCEF_NAV_CAN_GO_BACK = 0,
  GOCEF_NAV_GO_BACK,
  GOCEF_NAV_CAN_GO_FORWARD,
  GOCEF_NAV_GO_FORWARD,
  GOCEF_NAV_RELOAD,
  GOCEF_NAV_RELOAD_NO_CACHE,
  GOCEF_NAV_STOP,
};

int gocef_nav(uint64_t handle, int op);

// ---------------------------------------------------------------------------
// Input. Coordinates are relative to the view's upper-left corner, in pixels.

void gocef_send_mouse_move(uint64_t handle, int x, int y, uint32_t modifiers,
                           int leave);
void gocef_send_mouse_click(uint64_t handle, int x, int y, uint32_t modifiers,
                            int button, int mouseUp, int clickCount);
void gocef_send_mouse_wheel(uint64_t handle, int x, int y, uint32_t modifiers,
                            int deltaX, int deltaY);

// gocef_send_key sends one key event. keyType is a cef_key_event_type_t
// (0 = raw key down, 1 = key down, 2 = key up, 3 = char).
void gocef_send_key(uint64_t handle, int keyType, uint32_t modifiers,
                    int windowsKeyCode, int nativeKeyCode, uint16_t character,
                    uint16_t unmodifiedCharacter, int isSystemKey);

// ---------------------------------------------------------------------------
// DevTools protocol.
//
// This is the whole bridge surface: raw UTF-8 JSON in and out. CEF's other
// entry point, execute_dev_tools_method, takes a cef_dictionary_value_t, which
// would mean building a CEF value tree from Go for no gain — the protocol is
// JSON at both ends anyway.

// gocef_send_devtools sends one protocol message. Returns 0 on failure.
int gocef_send_devtools(uint64_t handle, const void *msg, size_t n);

// gocefOnDevToolsMessage delivers a protocol message: a result for one of our
// calls, or an event. The buffer is valid only for the duration of the call.
extern void gocefOnDevToolsMessage(uint64_t handle, void *msg, int n);

// ---------------------------------------------------------------------------
// Callbacks into Go. All are called on CEF's browser UI thread, which is the
// thread that pumps the loop.
//
// String parameters are UTF-8 buffers valid only for the duration of the call;
// a NULL pointer with length 0 is an empty string.

// gocefOnLoadingStateChange reports the browser-wide load state flipping. It
// fires exactly twice per navigation — loading and then settled — and url is
// the main frame's URL at that moment.
extern void gocefOnLoadingStateChange(uint64_t handle, int isLoading,
                                      void *url, int urlLen);

// gocefOnLoadError reports a navigation that failed or was cancelled.
// errorCode is a Chromium net error (see net_error_list.h); a cancelled
// navigation reports ERR_ABORTED (-3).
extern void gocefOnLoadError(uint64_t handle, int isMainFrame, int errorCode,
                             void *errorText, int errorTextLen,
                             void *url, int urlLen);

// gocefOnBeforeBrowse is consulted before a navigation starts. The engine is
// blocked on the answer. Return nonzero to allow the navigation, 0 to cancel
// it — note this is the Go sense ("may it proceed?"), inverted from CEF's
// on_before_browse return value.
extern int gocefOnBeforeBrowse(uint64_t handle, void *url, int urlLen,
                               uint32_t transition, int isMainFrame,
                               int userGesture, int isRedirect);

// gocefOnBeforePopup reports the page asking for a new window — target=_blank
// or window.open. The popup is always refused on CEF's side: a windowless
// browser has nowhere to host a second window, and letting CEF create one
// against this shim's client would conflate two browsers on one handle. The
// handler decides what becomes of the URL (typically loading it in place).
extern void gocefOnBeforePopup(uint64_t handle, void *url, int urlLen,
                               int userGesture);

// gocefOnPaint delivers a CPU frame: BGRA, upper-left origin, w*h*4 bytes,
// valid only for the duration of the call.
extern void gocefOnPaint(uint64_t handle, void *buffer, int w, int h, int isPopup);

// gocefOnAcceleratedPaint delivers a dma-buf frame. The fd is valid ONLY inside
// this call: CEF documents that the resource "cannot be cached and cannot be
// accessed outside of this callback" and that its contents "should be copied to
// a texture owned by the client application" (belluikit/bellui#113).
extern void gocefOnAcceleratedPaint(uint64_t handle, int planeCount, int fd,
                                    uint32_t stride, uint64_t modifier, int format);

// gocefOnPopupShow and gocefOnPopupSize report the separate surface CEF uses for
// <select> dropdowns. A host that ignores them renders dropdowns invisible.
extern void gocefOnPopupShow(uint64_t handle, int show);
extern void gocefOnPopupSize(uint64_t handle, int x, int y, int w, int h);

// gocefOnCursorChange reports a requested cursor as a cef_cursor_type_t.
extern void gocefOnCursorChange(uint64_t handle, int cursorType);

// gocefGetViewRect asks the host for the current view size in pixels. CEF
// requires a non-empty rectangle, so the Go side must always answer.
extern void gocefGetViewRect(uint64_t handle, int *w, int *h);

// gocefGetScreenScale asks the host for the display's device scale factor — the
// ratio between physical and logical pixels.
//
// This is what makes a HiDPI display render sharply. CEF's compositor decides
// the raster resolution from it, so a host that never supplies it gets content
// rasterised at 1x and then scaled up: soft text and blurred hairlines that
// nothing downstream can repair. It is pulled per call rather than captured
// once because a window can move between monitors of different scales.
extern float gocefGetScreenScale(uint64_t handle);

// gocefOnAfterCreated and gocefOnBeforeClose bracket the browser's life.
extern void gocefOnAfterCreated(uint64_t handle);
extern void gocefOnBeforeClose(uint64_t handle);

#endif // GOCEF_SHIM_H
