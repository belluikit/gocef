//go:build linux && cgo

package gocef_test

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
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

	cachePath := t.TempDir()
	if err := gocef.Initialize(gocef.Config{
		Dir:        dir,
		HelperPath: dir + "/cef_helper",
		CachePath:  cachePath,
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
	testProfileOnDisk(t, cachePath)
	testResize(t)
	testHiDPI(t)
	testVisibility(t)
	testNavigation(t)
	testIME(t)
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

// testProfileOnDisk asserts Config.CachePath reached CEF's cache_path. The
// discriminator is the "Default" profile directory Chromium creates under a
// non-empty cache_path and nowhere else: when only root_cache_path is set —
// the defect this section guards against — the directory holds installation
// data but no profile, and browsers silently run incognito. Cross-process
// persistence itself (cookies surviving a restart) cannot be tested here,
// since CEF initializes once per process; bellui's hack/cefhostprobe carries
// that half.
func testProfileOnDisk(t *testing.T, cachePath string) {
	t.Log("--- section: profile on disk")
	ok := pump(func() bool {
		st, err := os.Stat(cachePath + "/Default")
		return err == nil && st.IsDir()
	}, 10*time.Second)
	if !ok {
		entries, _ := os.ReadDir(cachePath)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("no Default profile directory appeared under CachePath within 10s; "+
			"cache_path was not honoured and the profile is in-memory (dir holds %v)", names)
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
// testVisibility exercises the one lifecycle call nothing else touches: a
// windowless browser has no window whose visibility CEF can observe, so
// SetVisible is what a host drives from show/hide — hidden must stop frames,
// and showing again must restart them without recreating anything.
func testVisibility(t *testing.T) {
	t.Log("--- section: visibility")
	h := newHandler("visibility", 320, 240, 1)
	b, err := gocef.NewBrowser(probePage, 320, 240, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h)

	if !pump(func() bool { _, ok := h.lastFrame(); return ok }, 20*time.Second) {
		t.Fatal("no initial frame")
	}

	b.SetVisible(false)
	// Drain deliveries already in flight, then measure quiet.
	pump(func() bool { return false }, 300*time.Millisecond)
	before := h.frameCount()
	pump(func() bool { return false }, 700*time.Millisecond)
	if after := h.frameCount(); after != before {
		t.Errorf("hidden browser painted %d more frames; WasHidden is not reaching CEF", after-before)
	}

	b.SetVisible(true)
	b.Invalidate()
	resumed := pump(func() bool { return h.frameCount() > before }, 10*time.Second)
	if !resumed {
		t.Error("no frame after SetVisible(true) + Invalidate; the browser did not wake")
	}
}

// navHandler adds the optional LoadHandler and NavigationHandler capabilities
// to the base test handler, recording what arrives and refusing whatever the
// test put in deny.
type navHandler struct {
	*handler

	mu      sync.Mutex
	loads   []loadEvent
	errors  []loadErrEvent
	browses []gocef.NavigationEvent
	deny    string // substring of URLs to refuse; empty refuses nothing
}

type loadEvent struct {
	loading bool
	url     string
}

type loadErrEvent struct {
	mainFrame bool
	code      int
	text, url string
}

func (h *navHandler) LoadingStateChanged(loading bool, url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loads = append(h.loads, loadEvent{loading: loading, url: url})
}

func (h *navHandler) LoadError(mainFrame bool, code int, text, url string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errors = append(h.errors, loadErrEvent{mainFrame: mainFrame, code: code, text: text, url: url})
}

func (h *navHandler) BeforeBrowse(ev gocef.NavigationEvent) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.browses = append(h.browses, ev)
	return h.deny == "" || !strings.Contains(ev.URL, h.deny)
}

func (h *navHandler) BeforePopup(string, bool) {}

// settled matches by substring: Chromium may normalise the URLs it reports,
// so an exact comparison against what LoadURL was given is not a contract.
func (h *navHandler) settled(urlPart string) func() bool {
	return func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, l := range h.loads {
			if !l.loading && strings.Contains(l.url, urlPart) {
				return true
			}
		}
		return false
	}
}

func (h *navHandler) sawBrowse(urlPart string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.browses {
		if strings.Contains(b.URL, urlPart) {
			return true
		}
	}
	return false
}

func (h *navHandler) abortedError(urlPart string) func() bool {
	return func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, e := range h.errors {
			if e.mainFrame && e.code == gocef.ErrAborted && strings.Contains(e.url, urlPart) {
				return true
			}
		}
		return false
	}
}

// testNavigation exercises the load handler and the navigation veto: an
// allowed navigation must report loading and settling with its URL, and a
// refused one must never settle — reporting ERR_ABORTED instead, which is the
// documented consequence of cancelling in on_before_browse.
func testNavigation(t *testing.T) {
	t.Log("--- section: navigation")
	const (
		pageTwo    = `data:text/html,<body style="background:%230000ff">two`
		pageDenied = `data:text/html,<body style="background:%23ff0000">denied`
	)

	h := &navHandler{handler: newHandler("navigation", 320, 240, 1)}
	b, err := gocef.NewBrowser(probePage, 320, 240, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h.handler)

	// The initial load consults the policy like any other navigation.
	if !pump(h.settled("ff00aa"), 20*time.Second) {
		t.Fatal("the initial navigation never settled through LoadingStateChanged")
	}
	if !h.sawBrowse("ff00aa") {
		t.Error("BeforeBrowse was not consulted for the initial navigation")
	}

	b.LoadURL(pageTwo)
	if !pump(h.settled("two"), 20*time.Second) {
		t.Error("the allowed navigation to pageTwo never settled")
	}
	if !h.sawBrowse("two") {
		t.Error("BeforeBrowse was not consulted for LoadURL")
	}

	h.mu.Lock()
	h.deny = "denied"
	h.mu.Unlock()

	b.LoadURL(pageDenied)
	if !pump(h.abortedError("denied"), 20*time.Second) {
		t.Error("the refused navigation never reported ERR_ABORTED through LoadError")
	}
	if pump(h.settled("denied"), 500*time.Millisecond) {
		t.Error("the refused navigation settled anyway; the veto did not cancel it")
	}
}

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

// ---------------------------------------------------------------------------
// IME composition (belluikit/bellui#121).

// imeTestHandler adds the two optional capabilities testIME needs: a CDP
// channel to read page state back, and the composition-range reports.
type imeTestHandler struct {
	*handler
	imu     sync.Mutex
	replies map[int]json.RawMessage
	ranges  []gocef.Range
	bounds  int // rects seen across all reports
}

func (h *imeTestHandler) DevToolsMessage(msg []byte) {
	var m struct {
		ID     int `json:"id"`
		Result struct {
			Result struct {
				Value json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	if json.Unmarshal(msg, &m) != nil || m.ID == 0 {
		return
	}
	h.imu.Lock()
	defer h.imu.Unlock()
	if h.replies == nil {
		h.replies = make(map[int]json.RawMessage)
	}
	h.replies[m.ID] = m.Result.Result.Value
}

func (h *imeTestHandler) ImeCompositionRangeChanged(sel gocef.Range, bounds []gocef.Rect) {
	h.imu.Lock()
	defer h.imu.Unlock()
	h.ranges = append(h.ranges, sel)
	h.bounds += len(bounds)
}

func (h *imeTestHandler) reply(id int) (json.RawMessage, bool) {
	h.imu.Lock()
	defer h.imu.Unlock()
	v, ok := h.replies[id]
	return v, ok
}

func (h *imeTestHandler) boundsSeen() int {
	h.imu.Lock()
	defer h.imu.Unlock()
	return h.bounds
}

var imeEvalID = 100

// evalJS evaluates expr in the page and returns the JSON-encoded value.
func evalJS(t *testing.T, b *gocef.Browser, h *imeTestHandler, expr string) string {
	t.Helper()
	imeEvalID++
	id := imeEvalID
	msg := fmt.Sprintf(`{"id":%d,"method":"Runtime.evaluate","params":{"expression":%q,"returnByValue":true}}`, id, expr)
	if !b.SendDevTools([]byte(msg)) {
		t.Fatalf("SendDevTools refused %s", msg)
	}
	var out json.RawMessage
	if !pump(func() bool { v, ok := h.reply(id); out = v; return ok }, 10*time.Second) {
		t.Fatalf("no CDP reply for %s", expr)
	}
	// CDP escapes non-ASCII in its JSON (に…); decode string values so
	// callers compare plain Go strings. Non-strings come back as raw JSON.
	var s string
	if json.Unmarshal(out, &s) == nil {
		return s
	}
	return string(out)
}

func testIME(t *testing.T) {
	t.Log("--- section: IME composition")

	// The page records every composition event; the input autofocuses so the
	// composition has a target node.
	const imePage = `data:text/html,<input id="t" autofocus>` +
		`<script>window.ev=[];` +
		`['compositionstart','compositionupdate','compositionend'].forEach(function(n){` +
		`document.getElementById('t').addEventListener(n,function(e){window.ev.push(n)});});` +
		`</script>`

	h := &imeTestHandler{handler: newHandler("ime", 320, 240, 1)}
	b, err := gocef.NewBrowser(imePage, 320, 240, false, h)
	if err != nil {
		t.Fatalf("NewBrowser: %v", err)
	}
	defer closeAndDrain(t, b, h.handler)

	if !pump(func() bool { _, got := h.lastFrame(); return got }, 20*time.Second) {
		t.Fatal("the IME page never painted")
	}
	b.SetFocus(true)
	if !pump(func() bool {
		return evalJS(t, b, h, "document.activeElement.id") == "t"
	}, 10*time.Second) {
		t.Fatal("the input never took focus")
	}

	// Composition appears in the input's value without committing.
	b.ImeSetComposition("にほ", 2, nil)
	if !pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "にほ"
	}, 10*time.Second) {
		t.Error("the composition text never reached the input's value")
	}

	// Commit replaces the composition contents.
	b.ImeCommitText("日本")
	if !pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "日本"
	}, 10*time.Second) {
		t.Error("the committed text never replaced the composition")
	}
	if !pump(func() bool {
		return evalJS(t, b, h, "window.ev.indexOf('compositionend')>=0") == "true"
	}, 5*time.Second) {
		t.Error("no compositionend event was seen by the page")
	}

	// Cancel discards without applying.
	b.ImeSetComposition("x", 1, nil)
	pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "日本x"
	}, 10*time.Second)
	b.ImeCancelComposition()
	if !pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "日本"
	}, 10*time.Second) {
		t.Error("the cancelled composition was applied instead of discarded")
	}

	// Finish commits the composition contents as they stand.
	b.ImeSetComposition("かな", 2, nil)
	pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "日本かな"
	}, 10*time.Second)
	b.ImeFinishComposingText(false)
	if !pump(func() bool {
		return evalJS(t, b, h, "document.getElementById('t').value") == "日本かな" &&
			evalJS(t, b, h, "window.ev.filter(function(n){return n==='compositionend'}).length>=2") == "true"
	}, 10*time.Second) {
		t.Error("FinishComposingText did not commit the composition as it stood")
	}

	// The composition-range seam fired with real character bounds at least
	// once while a composition was active (the candidate-window anchor).
	if h.boundsSeen() == 0 {
		t.Error("ImeCompositionRangeChanged never reported character bounds")
	}
}
