# gocef

Go binding for the Chromium Embedded Framework's **C API**, resolved at
**runtime**. Written for [bellui](https://github.com/belluikit/bellui)'s Linux
web-view backend (belluikit/bellui#103), which renders offscreen and composites
the frames into bellui's own canvas.

## Two decisions the whole design follows from

**It binds the C API, not C++.** CEF's C API is exported directly from
`libcef.so`, so `libcef_dll_wrapper` and the C++ ABI are not involved at all.
Handlers are structs of function pointers filled in by `shim.c` — the same shape
as the hand-rolled COM vtables in bellui's WebView2 backend. The generated
`include/capi/*.h` headers are vendored (`internal/cefapi`), because CEF
generates them at build time and only `cef_base_capi.h` exists in its git tree.

**`libcef.so` is dlopen'd, never linked.** Nothing here needs CEF present to
build, so `go build` is hermetic and fast on a machine that has never seen a
Chromium payload — and a missing payload is a typed `ErrRuntimeMissing` at
`Load`, not a link error. This mirrors what bellui already does for the WebView2
runtime on Windows.

Consequences worth knowing:

- A program importing this package builds on macOS and Windows too; it compiles
  `cef_unsupported.go` there, where everything reports `ErrUnsupported`.
- `CGO_ENABLED=0` on Linux gets the same stub.

## The API version pin

`APIVersion = 13300`, and `version.go` explains the choice at length. In short:
CEF's C API is versioned, leaving it undefined selects the *experimental*
unversioned API which is compatible with nothing, and pinning low maximises the
range of `libcef.so` builds one compiled binding accepts — which is what makes a
security bump a payload swap rather than a port (belluikit/bellui#98).

None of the surface bound here is version-gated, which is what makes the floor
reachable. Before raising the pin, re-run the guard survey in `version.go`.

## Payload

This package is the binding only; it ships no Chromium. The payload —
`libcef.so`, the resources, the subprocess helper — is published by
[gocef-libs](https://github.com/belluikit/gocef-libs) (belluikit/bellui#104) and
bundled into an application by `bellui package`.

`Locate()` finds it: `$BELLUI_CEF_DIR`, then `<exe dir>/cef`, then `<exe dir>`.

Resources go **flat**, beside `libcef.so` — not in a nested `Resources/`
directory, which fails inside ICU with `Invalid file descriptor to ICU data
received`. The V8 snapshot blobs are the exception to look out for: they live in
the distribution's `Release/`, not its `Resources/`.

## Usage

```go
dir, err := gocef.Locate()
if err != nil { /* no payload: degrade, do not fail */ }

runtime.LockOSThread()          // see Threading
if err := gocef.Load(dir); err != nil { return err }
if err := gocef.Initialize(gocef.Config{
    Dir:        dir,
    HelperPath: filepath.Join(dir, "cef_helper"),
}); err != nil { return err }
defer gocef.Shutdown()

b, err := gocef.NewBrowser("https://example.com", 800, 600, false, myHandler)
for { gocef.Work() }            // pump, on the same thread
```

## Threading — the trap that cost the most

CEF's browser UI thread is whichever thread called `Initialize`, and **every**
later call must come from it. Go does not promise that: the main goroutine
migrates between OS threads at any preemption point, and a pump loop that sleeps
is nothing but preemption points.

Without `runtime.LockOSThread()` the #60 probe delivered a first frame on roughly
one run in three, **with byte-identical logs whether it worked or not**. It reads
like a GPU fault or a CEF bug and is neither.

## Other traps, all recorded from real failures

- **The API-version handshake is mandatory, and `cef_api_hash` performs it** —
  despite reading like a query. Its doc says it "configures the CEF API version
  ... any changes to this value will be ignored after the first call". Skip it
  and `cef_api_version()` reports `-1` and the first struct handed across is
  fatally rejected, naming whatever type it happened to be validating. `Load`
  does this; a helper binary must do it too.
- **`cef_initialize` is once per process** — and cannot be called again after
  `Shutdown`. Re-initializing core-dumps.
- **The subprocess helper must not be the Go binary.** The Go runtime starts
  threads before `main` and Chromium's zygote forks. Ship the C helper in
  `helper/`; it finds `libcef.so` beside itself via `/proc/self/exe`, so it needs
  no rpath and no environment.
- **A windowless browser must be told it is visible.** It has no window whose
  visibility CEF can observe, so drive `SetVisible` from show/hide and from
  scrolling out of a clipping ancestor.

## HiDPI

`Handler.ScreenScale` is not optional detail. CEF's compositor decides its raster
resolution from the device scale factor, so a host that answers 1 on a scaled
display gets content rasterised at 1x and then scaled up — soft text, blurred
hairlines — and nothing downstream can repair it, because the pixels were never
drawn. It is asked per call rather than captured once, since a window can move
between monitors of different scales.

The screen rectangles are deliberately left equal to the view: CEF falls back to
`get_view_rect` when they are absent, which is right for an embedded view that
has no monitor of its own. They only affect where popups are placed, and a popup
belongs inside the view here.

## Frames

`Handler.Paint` delivers BGRA with an upper-left origin, valid only for the
duration of the call.

`Handler.AcceleratedPaint` delivers a dma-buf when shared textures are enabled —
verified on an RTX 2070 SUPER under X11 as single-plane `BGRA_8888` /
`DRM_FORMAT_MOD_LINEAR`, and needing both a live display session and
`--enable-features=Vulkan --use-vulkan=native`. **It is not a zero-copy
handoff**: CEF requires the contents be copied into a texture you own, inside
the callback (belluikit/bellui#113).

## Status

The design is proven end to end by bellui's `hack/cefprobe`, which is where every
trap above was found. This package is the extraction of that probe into a
reusable binding.

Implemented: the runtime loader and version handshake, windowless browser
lifecycle, resize, visibility, focus, the render callbacks (paint, accelerated
paint, popup surface, cursor), HiDPI scale, mouse and keyboard input,
navigation, and the DevTools bridge.

Not implemented: IME composition and touch (belluikit/bellui#121), and
`external_message_pump` (belluikit/bellui#120).

**Nothing here has run.** The portable build is verified and CI compiles the
Linux half, but no part of this has been exercised against a real CEF payload —
including the 13300 API pin, which is reasoned from a survey of version guards
where the probe only ever proved 15101. That is belluikit/bellui#117.

## Licence

The binding is ours. The vendored headers under `internal/cefapi/include` are
CEF's, BSD-3-Clause — see `internal/cefapi/LICENSE.txt`.
