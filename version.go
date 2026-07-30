package gocef

// APIVersion is the CEF API version this binding is compiled against, and it is
// the single most consequential constant in the package.
//
// # Why it is pinned at all
//
// CEF's C API is versioned. Leaving CEF_API_VERSION undefined selects
// CEF_API_VERSION_EXPERIMENTAL, which the headers describe as "not
// back/forward compatible with different CEF versions" — so an unversioned
// binding is welded to the exact CEF build it was compiled against, and every
// payload bump means re-vendoring headers and rebuilding. Pinning an explicit
// version is what makes one binding work across a range of libcef builds, which
// is the difference between a routine security bump and a porting exercise
// (belluikit/bellui#98).
//
// # Why this value
//
// 13300 is CEF_API_VERSION_MIN in the 151.3.12 distribution these headers came
// from, and the binding deliberately sits at the floor rather than the ceiling:
// the lower the pin, the wider the range of libcef builds one compiled binding
// accepts. That costs nothing here, because none of the surface this package
// binds is version-gated. Checked against the vendored headers:
//
//	cef_render_handler_capi.h    no CEF_API_ADDED guards at all
//	cef_client_capi.h            none
//	cef_life_span_handler_capi.h none
//	cef_request_handler_capi.h   none
//	cef_browser_capi.h           one, and it is CEF_EXPERIMENTAL (a CDP
//	                             accessibility control this package does not use)
//	cef_app_capi.h               one, at 14100 (entering a native message loop —
//	                             not used)
//	cef_settings_t               one, at 14600 (use_views_default_popup — not
//	                             used; pinning below it simply makes the struct
//	                             the size that version expects, which is correct)
//
// Before raising this, re-run those greps: the pin may only move up to reach API
// that is actually needed, and moving it up narrows the compatible range.
//
// # Not yet verified on hardware
//
// The #60 phase-0 probe proved the runtime handshake at 15101, because that is
// what its payload shipped. 13300 is reasoned from the guards above, not
// measured. If it is wrong it fails loudly and immediately — libcef rejects the
// first struct handed to it, the way a missing handshake does — rather than
// corrupting anything. Verify on the next Linux run.
const APIVersion = 13300

// Pinned records the CEF build these headers were taken from. It is provenance,
// not a runtime requirement: the whole point of APIVersion is that libcef need
// not be this exact build.
const (
	PinnedCEFVersion      = "151.3.12+gd9cea67+chromium-151.0.7922.47"
	PinnedChromiumVersion = "151.0.7922.47"
)
