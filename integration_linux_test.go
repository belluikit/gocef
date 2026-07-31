//go:build linux && cgo

package gocef_test

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/belluikit/gocef"
)

// This is the runtime pass for belluikit/bellui#117: everything in this package
// had been compile-verified and never run.
//
// It is one test function on purpose. CEF can only be initialized once per
// process, and every call must come from the thread that initialized it — a Go
// test binary runs test functions on whatever goroutine it likes, so spreading
// this across several tests would either break the thread rule or need a
// dispatch loop that obscures what is being tested.
//
// Skipped unless GOCEF_PAYLOAD names a directory containing libcef.so, so
// `go test ./...` stays runnable on a machine with no Chromium.

const (
	// A solid, unmistakable colour, so "did the page actually render" is a
	// pixel comparison rather than a judgement.
	pageColorR, pageColorG, pageColorB = 0xff, 0x00, 0xaa
	probePage                          = `data:text/html,<body style="margin:0;background:%23ff00aa">`
)

type frame struct {
	w, h  int
	pix   []byte
	popup bool
}

type handler struct {
	mu      sync.Mutex
	name    string
	w, h    int
	scale   float32
	frames  []frame
	created bool
	closed  bool
}

func newHandler(name string, w, h int, scale float32) *handler {
	return &handler{name: name, w: w, h: h, scale: scale}
}

func (h *handler) ViewSize() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.w, h.h
}

func (h *handler) setViewSize(w, hh int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.w, h.h = w, hh
}

func (h *handler) ScreenScale() float32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.scale
}

func (h *handler) Paint(buf []byte, w, hh int, popup bool) {
	// The buffer is only valid for the duration of the call, so it is copied
	// here — which is also what makes a use-after-free show up as a wrong
	// pixel rather than as luck.
	cp := make([]byte, len(buf))
	copy(cp, buf)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.frames = append(h.frames, frame{w: w, h: hh, pix: cp, popup: popup})
}

func (h *handler) AcceleratedPaint(gocef.AcceleratedFrame) {}
func (h *handler) PopupShow(bool)                          {}
func (h *handler) PopupRect(int, int, int, int)            {}
func (h *handler) CursorChange(int)                        {}

func (h *handler) Created() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.created = true
}

func (h *handler) Closed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}

func (h *handler) lastFrame() (frame, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.frames) - 1; i >= 0; i-- {
		if !h.frames[i].popup {
			return h.frames[i], true
		}
	}
	return frame{}, false
}

func (h *handler) frameCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.frames)
}

func (h *handler) wasCreated() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created
}

func (h *handler) wasClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func TestCEFRuntime(t *testing.T) {
	dir := os.Getenv("GOCEF_PAYLOAD")
	if dir == "" {
		t.Skip("GOCEF_PAYLOAD is unset; see the workflow for how the payload is staged")
	}

	// CEF's browser UI thread is whichever thread initialized it, and every
	// later call has to come from that thread. Without this the binding
	// delivers a first frame on roughly one run in three, with identical logs
	// either way (belluikit/bellui#102).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Load performs the API-version handshake, so a wrong APIVersion fails
	// here — loudly, which is the whole point of pinning it explicitly.
	if err := gocef.Load(dir); err != nil {
		t.Fatalf("Load: %v (this is where a wrong APIVersion=%d surfaces)", err, gocef.APIVersion)
	}

	switches := []string{"--no-sandbox"}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		// Chromium's ozone layer tries X11 otherwise and exits before anything
		// interesting happens.
		switches = append(switches, "--ozone-platform=headless")
	}

	// Chromium's own log is the diagnostic for a browser that silently does
	// nothing; on failure the tail is dumped alongside the callback counters.
	cefLog := t.TempDir() + "/cef.log"

	if err := gocef.Initialize(gocef.Config{
		Dir:        dir,
		HelperPath: dir + "/cef_helper",
		CachePath:  t.TempDir(),
		Switches:   switches,
		LogFile:    cefLog,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer gocef.Shutdown()

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("shim callback counters: %+v", gocef.ReadStats())
		if data, err := os.ReadFile(cefLog); err == nil {
			const keep = 8000
			if len(data) > keep {
				data = data[len(data)-keep:]
			}
			t.Logf("CEF log tail:\n%s", data)
		} else {
			t.Logf("no CEF log was written (%v)", err)
		}
	})

	// A second Initialize must be refused rather than attempted: CEF cannot be
	// initialized twice in a process and doing so core-dumps.
	if err := gocef.Initialize(gocef.Config{Dir: dir}); err == nil {
		t.Error("second Initialize succeeded; it must be refused, since CEF is once-per-process")
	}

	// NOT t.Run. Subtests execute on a fresh goroutine, which lands on some
	// other OS thread — and every CEF call must come from the thread that
	// called Initialize. With subtests, create_browser posts its task to the
	// real UI thread and do_message_loop_work pumps a different one: creation
	// "succeeds" and then nothing ever happens, with all shim counters at
	// zero. The probe's thread trap, re-entered through the testing package.
	testFrames(t)
	testResize(t)
	testHiDPI(t)
	testTwoBrowsers(t)
}

// pump runs CEF's loop until cond holds. Callbacks are delivered inside Work,
// so this is also what makes them happen.
func pump(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		gocef.Work()
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// closeAndDrain closes a browser and pumps until CEF reports it gone, so a
// leaked context or a double free surfaces here rather than in a later test.
func closeAndDrain(t *testing.T, b *gocef.Browser, h *handler) {
	t.Helper()
	b.Close()
	if !pump(h.wasClosed, 10*time.Second) {
		t.Error("Closed callback never fired after Close")
	}
}

func testFrames(t *testing.T) {
	t.Log("--- section: frames")
	h := newHandler("frames", 400, 300, 1)
	b, err := gocef.NewBrowser(probePage, 400, 300, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h)

	if !pump(h.wasCreated, 20*time.Second) {
		t.Fatal("Created callback never fired")
	}

	// Wait for a frame carrying the page, not merely a frame: the compositor's
	// first delivery can be its initial clear, painted before the renderer has
	// produced the document — on a fast machine that frame arrives first and a
	// pixel assertion against it fails black. Content is the condition, and it
	// doubles as the BGRA byte-order check: a channel swap can never match.
	ok := pump(func() bool {
		f, got := h.lastFrame()
		if !got || len(f.pix) < 4 {
			return false
		}
		return f.pix[0] == pageColorB && f.pix[1] == pageColorG && f.pix[2] == pageColorR
	}, 20*time.Second)

	f, got := h.lastFrame()
	if !got {
		t.Fatal("no frame arrived through Paint within 20s")
	}
	if f.w != 400 || f.h != 300 {
		t.Errorf("frame is %dx%d, want 400x300", f.w, f.h)
	}
	if got, want := len(f.pix), f.w*f.h*4; got != want {
		t.Fatalf("buffer is %d bytes, want %d (w*h*4)", got, want)
	}
	if !ok {
		bl, gr, rd := f.pix[0], f.pix[1], f.pix[2]
		t.Errorf("no frame ever matched the page colour; last first-pixel is "+
			"B=%#x G=%#x R=%#x, want B=%#x G=%#x R=%#x — either the page never "+
			"rendered or the channel order is not BGRA",
			bl, gr, rd, pageColorB, pageColorG, pageColorR)
	}
}

func testResize(t *testing.T) {
	t.Log("--- section: resize")
	h := newHandler("resize", 320, 240, 1)
	b, err := gocef.NewBrowser(probePage, 320, 240, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h)

	if !pump(func() bool { _, ok := h.lastFrame(); return ok }, 20*time.Second) {
		t.Fatal("no initial frame")
	}

	// CEF re-reads the size through ViewSize, so the handler has to change
	// before Resize is called — that ordering is part of the contract.
	h.setViewSize(500, 380)
	b.Resize(500, 380)

	ok := pump(func() bool {
		f, ok := h.lastFrame()
		return ok && f.w == 500 && f.h == 380
	}, 20*time.Second)
	if !ok {
		f, _ := h.lastFrame()
		t.Errorf("no frame at the new size within 20s; last frame is %dx%d, want 500x380", f.w, f.h)
	}
}

// testHiDPI is the assertion behind belluikit/bellui#119. A device scale factor
// of 2 must make CEF rasterise at twice the view size — that is the whole
// mechanism, and if the scale never reaches the compositor the buffer comes
// back at 1x and the only symptom on screen is soft text.
func testHiDPI(t *testing.T) {
	t.Log("--- section: hidpi")
	const viewW, viewH = 200, 150
	h := newHandler("hidpi", viewW, viewH, 2)
	b, err := gocef.NewBrowser(probePage, viewW, viewH, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h)

	ok := pump(func() bool {
		f, ok := h.lastFrame()
		return ok && f.w >= viewW*2
	}, 20*time.Second)
	if !ok {
		f, _ := h.lastFrame()
		t.Errorf("at scale 2 the frame is %dx%d, want %dx%d — the device scale "+
			"factor is not reaching CEF's compositor", f.w, f.h, viewW*2, viewH*2)
	}
}

// testTwoBrowsers is what the per-browser handler contexts exist for. The
// phase-0 probe only ever created one browser and used file-scope singletons
// with inert refcounts; this is the first thing that would break if the
// container_of recovery or the refcounting were wrong.
func testTwoBrowsers(t *testing.T) {
	t.Log("--- section: two browsers")
	var hs []*handler
	var bs []*gocef.Browser
	for i := range 2 {
		h := newHandler(fmt.Sprintf("browser-%d", i), 200+i*40, 150+i*30, 1)
		b, err := gocef.NewBrowser(probePage, 200+i*40, 150+i*30, false, h)
		if err != nil {
			t.Fatalf("NewBrowser %d: %v", i, err)
		}
		hs = append(hs, h)
		bs = append(bs, b)
	}

	ok := pump(func() bool {
		for _, h := range hs {
			if _, got := h.lastFrame(); !got {
				return false
			}
		}
		return true
	}, 30*time.Second)
	if !ok {
		for i, h := range hs {
			t.Logf("browser %d: created=%v frames=%d", i, h.wasCreated(), h.frameCount())
		}
		t.Fatal("both browsers did not produce a frame within 30s")
	}

	// Each browser must get its own size back. One shared context would show up
	// here as both frames carrying the same dimensions.
	for i, h := range hs {
		f, _ := h.lastFrame()
		wantW, wantH := 200+i*40, 150+i*30
		if f.w != wantW || f.h != wantH {
			t.Errorf("browser %d frame is %dx%d, want %dx%d — handler contexts are crossed",
				i, f.w, f.h, wantW, wantH)
		}
	}

	for i := range bs {
		closeAndDrain(t, bs[i], hs[i])
	}
}
