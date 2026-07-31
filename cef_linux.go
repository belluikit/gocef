//go:build linux && cgo

package gocef

/*
#cgo CFLAGS: -I${SRCDIR} -I${SRCDIR}/internal/cefapi -DCEF_API_VERSION=13300
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"
)

// Supported reports whether this build has a CEF binding.
const Supported = true

// ErrRuntimeMissing is reported when no CEF payload can be found or loaded.
//
// CEF is not a system component on any Linux distribution — no mainstream
// distro packages it, and it has no stable ABI, so it travels with the
// application rather than with the OS. An app that ships one will find it; an
// app that does not should handle this error and offer the content another way,
// exactly as a Windows app handles a missing WebView2 runtime.
var ErrRuntimeMissing = errors.New("gocef: no CEF payload found")

// ErrNotLoaded is reported by calls made before a successful Load.
var ErrNotLoaded = errors.New("gocef: library not loaded")

var (
	mu       sync.Mutex
	loaded   bool
	started  bool
	browsers = map[uint64]*Browser{}
	nextID   uint64
)

// Locate finds a CEF payload: a directory containing libcef.so.
//
// Search order, most specific first:
//
//	$BELLUI_CEF_DIR       an explicit override
//	<executable dir>/cef  the packaged layout
//	<executable dir>      libcef.so beside the binary
//
// This mirrors what the Windows backend does for the WebView2 runtime: prefer
// what the application shipped, fall back to what the machine has, and report a
// typed error rather than failing deep inside a call that is not there.
func Locate() (string, error) {
	if dir := os.Getenv("BELLUI_CEF_DIR"); dir != "" {
		if ok, _ := hasLibcef(dir); ok {
			return dir, nil
		}
		return "", fmt.Errorf("%w: BELLUI_CEF_DIR=%s has no libcef.so", ErrRuntimeMissing, dir)
	}
	exe, err := os.Executable()
	if err == nil {
		base := filepath.Dir(exe)
		for _, dir := range []string{filepath.Join(base, "cef"), base} {
			if ok, _ := hasLibcef(dir); ok {
				return dir, nil
			}
		}
	}
	return "", ErrRuntimeMissing
}

func hasLibcef(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, "libcef.so"))
	return err == nil, err
}

// Load resolves libcef.so out of dir and performs CEF's API-version handshake.
// Call it once, before Initialize.
func Load(dir string) error {
	mu.Lock()
	defer mu.Unlock()
	if loaded {
		return nil
	}

	// The Go constant and the -DCEF_API_VERSION the C is compiled with are two
	// declarations of the same fact; drift between them would surface as a
	// struct-size disagreement long after the cause.
	if got := int(C.gocef_compiled_api_version()); got != APIVersion {
		return fmt.Errorf("gocef: built with CEF_API_VERSION=%d but APIVersion is %d", got, APIVersion)
	}

	cdir := C.CString(dir)
	defer C.free(unsafe.Pointer(cdir))
	if C.gocef_load(cdir, C.int(APIVersion)) != 0 {
		return fmt.Errorf("%w: %s", ErrRuntimeMissing, C.GoString(C.gocef_error()))
	}
	loaded = true
	return nil
}

// Config describes the payload layout and the Chromium command line.
type Config struct {
	// Dir is the payload directory: libcef.so, the resources and the helper.
	Dir string

	// HelperPath is the subprocess executable. It must not be the Go binary:
	// the Go runtime starts threads before main and Chromium's zygote forks, so
	// a Go program cannot serve as its own Chromium subprocess.
	HelperPath string

	// CachePath is where Chromium keeps its profile. Empty means a temporary
	// one, discarded on exit.
	CachePath string

	// Switches are extra Chromium command-line switches. Chromium propagates
	// them to the subprocesses it launches, so they need setting only here.
	//
	// Two that matter on Linux: --ozone-platform=headless is required with no
	// display server, and dma-buf delivery needs
	// --enable-features=Vulkan --use-vulkan=native (belluikit/bellui#102).
	Switches []string

	// LogFile, when set, raises CEF's log severity to VERBOSE and writes
	// Chromium's own logging there — browser, renderer and GPU processes
	// alike. It is the diagnostic for a browser that silently does nothing,
	// which Chromium tends to explain only below WARNING.
	LogFile string
}

// Initialize starts CEF's browser process.
//
// It must be called from the thread that will call Work, and that thread must
// stay put: CEF's browser UI thread is whichever thread called this, and Go
// will migrate a goroutine between OS threads at any preemption point unless
// runtime.LockOSThread pins it. Getting this wrong produces a browser that
// delivers frames only sometimes, with identical logs either way.
//
// CEF cannot be initialized twice in one process, and cannot be initialized
// again after Shutdown.
func Initialize(cfg Config) error {
	mu.Lock()
	defer mu.Unlock()
	if !loaded {
		return ErrNotLoaded
	}
	if started {
		return errors.New("gocef: already initialized (CEF is once-per-process)")
	}

	res := C.CString(cfg.Dir)
	defer C.free(unsafe.Pointer(res))
	loc := C.CString(filepath.Join(cfg.Dir, "locales"))
	defer C.free(unsafe.Pointer(loc))
	cache := C.CString(cfg.CachePath)
	defer C.free(unsafe.Pointer(cache))
	helper := C.CString(cfg.HelperPath)
	defer C.free(unsafe.Pointer(helper))
	sw := C.CString(joinSwitches(cfg.Switches))
	defer C.free(unsafe.Pointer(sw))

	lf := C.CString(cfg.LogFile)
	defer C.free(unsafe.Pointer(lf))

	if C.gocef_initialize(res, loc, cache, helper, sw, 60, lf) != 0 {
		return fmt.Errorf("gocef: %s", C.GoString(C.gocef_error()))
	}
	started = true
	return nil
}

func joinSwitches(sw []string) string {
	out := ""
	for i, s := range sw {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// Work runs one iteration of CEF's message loop, and must be called from the
// thread that called Initialize. Callbacks are delivered inside it.
func Work() {
	C.gocef_work()
}

// Stats reports how many times each shim callback has fired, process-wide. Its
// use is diagnosing a browser that silently does nothing: ViewRect at zero
// means CEF never consulted the client at all, and nonzero counts walk the
// line to wherever progress stopped.
type Stats struct {
	ViewRect     int64
	ScreenInfo   int64
	Paint        int64
	AfterCreated int64
	BeforeClose  int64
}

// ReadStats returns the current callback counts.
func ReadStats() Stats {
	return Stats{
		ViewRect:     int64(C.gocef_counter(C.GOCEF_N_GET_VIEW_RECT)),
		ScreenInfo:   int64(C.gocef_counter(C.GOCEF_N_GET_SCREEN_INFO)),
		Paint:        int64(C.gocef_counter(C.GOCEF_N_PAINT)),
		AfterCreated: int64(C.gocef_counter(C.GOCEF_N_AFTER_CREATED)),
		BeforeClose:  int64(C.gocef_counter(C.GOCEF_N_BEFORE_CLOSE)),
	}
}

// Shutdown stops CEF. Nothing in this package may be used afterwards.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if !started {
		return
	}
	C.gocef_shutdown()
	started = false
}

// ExecuteProcess is the subprocess entry point, for a helper binary built from
// Go. It returns the exit code for a recognised Chromium subprocess, or -1 in
// the browser process.
//
// Prefer the prebuilt C helper: see Config.HelperPath for why a Go binary is a
// poor Chromium subprocess.
func ExecuteProcess(args []string) int {
	argv := make([]*C.char, len(args)+1)
	for i, a := range args {
		argv[i] = C.CString(a)
		defer C.free(unsafe.Pointer(argv[i]))
	}
	return int(C.gocef_execute_process(C.int(len(args)), &argv[0]))
}
