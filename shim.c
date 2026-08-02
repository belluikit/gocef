//go:build linux && cgo

// CEF C API shim. See shim.h for the shape and the two structural decisions.

// The API version is supplied by the build (see cef_linux.go's CFLAGS), which
// keeps a single definition in version.go rather than one here and one there.
#ifndef CEF_API_VERSION
#error CEF_API_VERSION must be defined by the build
#endif

#include "shim.h"

#include <dlfcn.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "include/capi/cef_app_capi.h"
#include "include/capi/cef_browser_capi.h"
#include "include/capi/cef_client_capi.h"
#include "include/capi/cef_devtools_message_observer_capi.h"
#include "include/capi/cef_display_handler_capi.h"
#include "include/capi/cef_life_span_handler_capi.h"
#include "include/capi/cef_load_handler_capi.h"
#include "include/capi/cef_render_handler_capi.h"
#include "include/capi/cef_request_handler_capi.h"

#define MAX_SWITCHES 64
#define MAX_BROWSERS 64

// ---------------------------------------------------------------------------
// libcef.so entry points, resolved at runtime.

static void *g_lib;
static char g_err[1400]; // fits a full path plus a dlerror without truncation

static _Atomic long g_counters[GOCEF_N_COUNTERS];

long gocef_counter(int which) {
  if (which < 0 || which >= GOCEF_N_COUNTERS) {
    return -1;
  }
  return atomic_load(&g_counters[which]);
}

#define COUNT(which) atomic_fetch_add(&g_counters[which], 1)

static const char *(*p_api_hash)(int, int);
static int (*p_api_version)(void);
static int (*p_initialize)(const cef_main_args_t *, const struct _cef_settings_t *,
                           cef_app_t *, void *);
static int (*p_execute_process)(const cef_main_args_t *, cef_app_t *, void *);
static void (*p_shutdown)(void);
static void (*p_do_message_loop_work)(void);
static int (*p_create_browser)(const cef_window_info_t *, struct _cef_client_t *,
                               const cef_string_t *,
                               const struct _cef_browser_settings_t *,
                               struct _cef_dictionary_value_t *,
                               struct _cef_request_context_t *);
static int (*p_utf8_to_utf16)(const char *, size_t, cef_string_t *);
static int (*p_utf16_to_utf8)(const char16_t *, size_t, cef_string_utf8_t *);
static void (*p_utf8_clear)(cef_string_utf8_t *);
static void (*p_userfree_utf16_free)(cef_string_userfree_utf16_t);

const char *gocef_error(void) { return g_err; }

int gocef_compiled_api_version(void) { return CEF_API_VERSION; }

static int resolve(void **slot, const char *name) {
  *slot = dlsym(g_lib, name);
  if (*slot == NULL) {
    snprintf(g_err, sizeof(g_err), "libcef.so has no symbol %s", name);
    return -1;
  }
  return 0;
}

static void setstr(cef_string_t *dst, const char *src) {
  if (src != NULL && src[0] != '\0') {
    p_utf8_to_utf16(src, strlen(src), dst);
  }
}

// to_utf8 converts a CEF UTF-16 string for the duration of a callback. The
// result must be released with utf8_free; a NULL or empty input converts to
// {NULL, 0}, which the Go side reads as "".
static cef_string_utf8_t to_utf8(const cef_string_t *s) {
  cef_string_utf8_t out;
  memset(&out, 0, sizeof(out));
  if (s != NULL && s->str != NULL && s->length > 0) {
    p_utf16_to_utf8(s->str, s->length, &out);
  }
  return out;
}

static void utf8_free(cef_string_utf8_t *s) { p_utf8_clear(s); }

// userfree_to_utf8 is to_utf8 for the strings CEF getters return with
// ownership — it converts and then frees the original in one step.
static cef_string_utf8_t userfree_to_utf8(cef_string_userfree_t s) {
  cef_string_utf8_t out = to_utf8(s);
  if (s != NULL) {
    p_userfree_utf16_free(s);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Per-browser state.
//
// The four handler structs CEF needs are allocated together with the handle
// they belong to, so a callback can recover the owning context by subtracting
// the member's offset. That is what lets one shim serve many browsers without a
// lookup table on the C side.
//
// Unlike the phase-0 probe — which used file-scope singletons and inert
// refcounts because it created exactly one browser and never freed it — these
// are real counts. CEF holds references to each handler independently and
// releases them at times we do not control, so the context is freed when the
// last one goes.

typedef struct {
  cef_client_t client;
  cef_render_handler_t render;
  cef_life_span_handler_t life;
  cef_display_handler_t display;
  cef_load_handler_t load;
  cef_request_handler_t request;
  cef_dev_tools_message_observer_t devtools;

  atomic_int refs;
  uint64_t handle;
  cef_browser_t *browser;

  // Holding this registration is what keeps the observer subscribed: releasing
  // it unsubscribes, so it is kept for the browser's life rather than dropped
  // at the end of the call that created it.
  cef_registration_t *devtools_reg;
} ctx_t;

static ctx_t *g_ctx[MAX_BROWSERS];

#define CTX_OF(member, ptr) \
  ((ctx_t *)((char *)(ptr) - offsetof(ctx_t, member)))

static ctx_t *ctx_find(uint64_t handle) {
  for (int i = 0; i < MAX_BROWSERS; i++) {
    if (g_ctx[i] != NULL && g_ctx[i]->handle == handle) {
      return g_ctx[i];
    }
  }
  return NULL;
}

static void ctx_free(ctx_t *c) {
  for (int i = 0; i < MAX_BROWSERS; i++) {
    if (g_ctx[i] == c) {
      g_ctx[i] = NULL;
      break;
    }
  }
  free(c);
}

// Every handler's refcount slots funnel into the context's single count: they
// share a lifetime, so tracking them separately would buy nothing but bugs.
static void ctx_ref(ctx_t *c) { atomic_fetch_add(&c->refs, 1); }

static int ctx_unref(ctx_t *c) {
  if (atomic_fetch_sub(&c->refs, 1) == 1) {
    ctx_free(c);
    return 1;
  }
  return 0;
}

#define REFCOUNT_IMPL(name, member)                                        \
  static void CEF_CALLBACK name##_add_ref(cef_base_ref_counted_t *self) {  \
    ctx_ref(CTX_OF(member, self));                                         \
  }                                                                        \
  static int CEF_CALLBACK name##_release(cef_base_ref_counted_t *self) {   \
    return ctx_unref(CTX_OF(member, self));                                \
  }                                                                        \
  static int CEF_CALLBACK name##_has_one(cef_base_ref_counted_t *self) {   \
    return atomic_load(&CTX_OF(member, self)->refs) == 1;                  \
  }                                                                        \
  static int CEF_CALLBACK name##_has_any(cef_base_ref_counted_t *self) {   \
    return atomic_load(&CTX_OF(member, self)->refs) > 0;                   \
  }

REFCOUNT_IMPL(client, client)
REFCOUNT_IMPL(render, render)
REFCOUNT_IMPL(life, life)
REFCOUNT_IMPL(display, display)
REFCOUNT_IMPL(load, load)
REFCOUNT_IMPL(request, request)
REFCOUNT_IMPL(devtools, devtools)

// init_base sets the size field CEF reads to tell API versions apart. Getting
// it wrong truncates the struct silently, so it is always sizeof the real one.
static void init_base(cef_base_ref_counted_t *base, size_t size,
                      void (CEF_CALLBACK *add)(cef_base_ref_counted_t *),
                      int (CEF_CALLBACK *rel)(cef_base_ref_counted_t *),
                      int (CEF_CALLBACK *one)(cef_base_ref_counted_t *),
                      int (CEF_CALLBACK *any)(cef_base_ref_counted_t *)) {
  base->size = size;
  base->add_ref = add;
  base->release = rel;
  base->has_one_ref = one;
  base->has_at_least_one_ref = any;
}

// ---------------------------------------------------------------------------
// cef_render_handler_t

static void CEF_CALLBACK rh_get_view_rect(struct _cef_render_handler_t *self,
                                          struct _cef_browser_t *browser,
                                          cef_rect_t *rect) {
  (void)browser;
  ctx_t *c = CTX_OF(render, self);
  COUNT(GOCEF_N_GET_VIEW_RECT);
  int w = 0, h = 0;
  gocefGetViewRect(c->handle, &w, &h);
  // CEF requires a non-empty rectangle; a zero here is a hard failure inside
  // the compositor rather than an empty view.
  rect->x = 0;
  rect->y = 0;
  rect->width = w > 0 ? w : 1;
  rect->height = h > 0 ? h : 1;
}

static int CEF_CALLBACK rh_get_screen_info(struct _cef_render_handler_t *self,
                                           struct _cef_browser_t *browser,
                                           cef_screen_info_t *info) {
  (void)browser;
  ctx_t *c = CTX_OF(render, self);
  COUNT(GOCEF_N_GET_SCREEN_INFO);

  info->device_scale_factor = gocefGetScreenScale(c->handle);
  info->depth = 32;
  info->depth_per_component = 8;
  info->is_monochrome = 0;

  // Left as the view rectangle: CEF documents that a NULL screen rectangle
  // falls back to get_view_rect, which is right for an embedded view that has
  // no monitor of its own. The rectangles only matter for placing popups, and
  // a popup belongs inside the view here rather than inside a display.
  int w = 0, h = 0;
  gocefGetViewRect(c->handle, &w, &h);
  info->rect.x = 0;
  info->rect.y = 0;
  info->rect.width = w > 0 ? w : 1;
  info->rect.height = h > 0 ? h : 1;
  info->available_rect = info->rect;

  return 1; // the structure was filled in
}

static void CEF_CALLBACK rh_on_paint(struct _cef_render_handler_t *self,
                                     struct _cef_browser_t *browser,
                                     cef_paint_element_type_t type,
                                     size_t dirtyRectsCount,
                                     cef_rect_t const *dirtyRects,
                                     const void *buffer, int width, int height) {
  (void)browser;
  (void)dirtyRectsCount;
  (void)dirtyRects;
  COUNT(GOCEF_N_PAINT);
  gocefOnPaint(CTX_OF(render, self)->handle, (void *)buffer, width, height,
               type == PET_POPUP);
}

static void CEF_CALLBACK rh_on_accelerated_paint(
    struct _cef_render_handler_t *self, struct _cef_browser_t *browser,
    cef_paint_element_type_t type, size_t dirtyRectsCount,
    cef_rect_t const *dirtyRects, const cef_accelerated_paint_info_t *info) {
  (void)browser;
  (void)type;
  (void)dirtyRectsCount;
  (void)dirtyRects;
  ctx_t *c = CTX_OF(render, self);
  if (info == NULL || info->plane_count < 1) {
    gocefOnAcceleratedPaint(c->handle, 0, -1, 0, 0, 0);
    return;
  }
  gocefOnAcceleratedPaint(c->handle, info->plane_count, info->planes[0].fd,
                          info->planes[0].stride, info->modifier, info->format);
}

static void CEF_CALLBACK rh_on_popup_show(struct _cef_render_handler_t *self,
                                          struct _cef_browser_t *browser,
                                          int show) {
  (void)browser;
  gocefOnPopupShow(CTX_OF(render, self)->handle, show);
}

static void CEF_CALLBACK rh_on_popup_size(struct _cef_render_handler_t *self,
                                          struct _cef_browser_t *browser,
                                          const cef_rect_t *rect) {
  (void)browser;
  gocefOnPopupSize(CTX_OF(render, self)->handle, rect->x, rect->y, rect->width,
                   rect->height);
}

// ---------------------------------------------------------------------------
// cef_display_handler_t
//
// The cursor lives here, not on the render handler where an offscreen
// integration might expect it — a distinction the compiler is unusually clear
// about, since the member simply does not exist on cef_render_handler_t.

static int CEF_CALLBACK dh_on_cursor_change(struct _cef_display_handler_t *self,
                                            struct _cef_browser_t *browser,
                                            cef_cursor_handle_t cursor,
                                            cef_cursor_type_t type,
                                            const cef_cursor_info_t *info) {
  (void)browser;
  (void)cursor;
  (void)info;
  gocefOnCursorChange(CTX_OF(display, self)->handle, (int)type);
  // 1 = handled. The host owns the cursor because there is no CEF window to own
  // it, so default handling has nothing to act on.
  return 1;
}

// ---------------------------------------------------------------------------
// cef_load_handler_t
//
// on_loading_state_change is the one complete signal — it fires exactly twice
// per navigation, before any on_load_start and after every on_load_end or
// on_load_error — so the start/end pair is not bound at all. The URL is read
// from the main frame here because the callback itself carries no frame.

static void CEF_CALLBACK lh_on_loading_state_change(
    struct _cef_load_handler_t *self, struct _cef_browser_t *browser,
    int isLoading, int canGoBack, int canGoForward) {
  (void)canGoBack; // queried on demand through gocef_nav, not pushed
  (void)canGoForward;
  ctx_t *c = CTX_OF(load, self);

  cef_string_utf8_t url;
  memset(&url, 0, sizeof(url));
  cef_frame_t *frame = browser->get_main_frame(browser);
  if (frame != NULL) {
    url = userfree_to_utf8(frame->get_url(frame));
    frame->base.release(&frame->base);
  }
  gocefOnLoadingStateChange(c->handle, isLoading, url.str, (int)url.length);
  utf8_free(&url);
}

static void CEF_CALLBACK lh_on_load_error(struct _cef_load_handler_t *self,
                                          struct _cef_browser_t *browser,
                                          struct _cef_frame_t *frame,
                                          cef_errorcode_t errorCode,
                                          const cef_string_t *errorText,
                                          const cef_string_t *failedUrl) {
  (void)browser;
  ctx_t *c = CTX_OF(load, self);
  int isMain = frame != NULL ? frame->is_main(frame) : 1;
  cef_string_utf8_t text = to_utf8(errorText);
  cef_string_utf8_t url = to_utf8(failedUrl);
  gocefOnLoadError(c->handle, isMain, (int)errorCode, text.str,
                   (int)text.length, url.str, (int)url.length);
  utf8_free(&text);
  utf8_free(&url);
}

// ---------------------------------------------------------------------------
// cef_request_handler_t

static int CEF_CALLBACK rq_on_before_browse(struct _cef_request_handler_t *self,
                                            struct _cef_browser_t *browser,
                                            struct _cef_frame_t *frame,
                                            struct _cef_request_t *request,
                                            int user_gesture, int is_redirect) {
  (void)browser;
  ctx_t *c = CTX_OF(request, self);
  cef_string_utf8_t url = userfree_to_utf8(request->get_url(request));
  uint32_t transition = (uint32_t)request->get_transition_type(request);
  int allow = gocefOnBeforeBrowse(c->handle, url.str, (int)url.length,
                                  transition, frame->is_main(frame),
                                  user_gesture, is_redirect);
  utf8_free(&url);
  return allow ? 0 : 1; // CEF's sense: 1 cancels
}

// ---------------------------------------------------------------------------
// cef_life_span_handler_t

static int CEF_CALLBACK ls_on_before_popup(
    struct _cef_life_span_handler_t *self, struct _cef_browser_t *browser,
    struct _cef_frame_t *frame, int popup_id, const cef_string_t *target_url,
    const cef_string_t *target_frame_name,
    cef_window_open_disposition_t target_disposition, int user_gesture,
    const cef_popup_features_t *popupFeatures,
    struct _cef_window_info_t *windowInfo, struct _cef_client_t **client,
    struct _cef_browser_settings_t *settings,
    struct _cef_dictionary_value_t **extra_info, int *no_javascript_access) {
  (void)browser;
  (void)frame;
  (void)popup_id;
  (void)target_frame_name;
  (void)target_disposition;
  (void)popupFeatures;
  (void)windowInfo;
  (void)client;
  (void)settings;
  (void)extra_info;
  (void)no_javascript_access;
  ctx_t *c = CTX_OF(life, self);
  cef_string_utf8_t url = to_utf8(target_url);
  gocefOnBeforePopup(c->handle, url.str, (int)url.length, user_gesture);
  utf8_free(&url);
  // Always cancelled: a windowless browser has nowhere to host a second
  // window, and letting CEF create one against this client would conflate two
  // browsers on one handle. See shim.h.
  return 1;
}

static void CEF_CALLBACK ls_on_after_created(struct _cef_life_span_handler_t *self,
                                             struct _cef_browser_t *browser) {
  ctx_t *c = CTX_OF(life, self);
  COUNT(GOCEF_N_AFTER_CREATED);
  browser->base.add_ref(&browser->base);
  c->browser = browser;

  // The observer can only be registered once there is a host, which is why this
  // happens here rather than at creation.
  cef_browser_host_t *host = browser->get_host(browser);
  if (host != NULL) {
    c->devtools_reg = host->add_dev_tools_message_observer(host, &c->devtools);
    host->base.release(&host->base);
  }

  gocefOnAfterCreated(c->handle);
}

static void CEF_CALLBACK ls_on_before_close(struct _cef_life_span_handler_t *self,
                                            struct _cef_browser_t *browser) {
  (void)browser;
  ctx_t *c = CTX_OF(life, self);
  COUNT(GOCEF_N_BEFORE_CLOSE);
  if (c->devtools_reg != NULL) {
    c->devtools_reg->base.release(&c->devtools_reg->base);
    c->devtools_reg = NULL;
  }
  if (c->browser != NULL) {
    c->browser->base.release(&c->browser->base);
    c->browser = NULL;
  }
  gocefOnBeforeClose(c->handle);
}

// ---------------------------------------------------------------------------
// cef_dev_tools_message_observer_t
//
// The whole bridge rides on this one callback plus send_dev_tools_message:
// UTF-8 JSON in, UTF-8 JSON out. Returning 1 would consume the message and stop
// CEF delivering it to the more specific callbacks below it; 0 leaves normal
// delivery alone, which is what a passive observer wants.

static int CEF_CALLBACK dt_on_message(struct _cef_dev_tools_message_observer_t *self,
                                      struct _cef_browser_t *browser,
                                      const void *message, size_t message_size) {
  (void)browser;
  gocefOnDevToolsMessage(CTX_OF(devtools, self)->handle, (void *)message,
                         (int)message_size);
  return 0;
}

// ---------------------------------------------------------------------------
// cef_client_t

// The getters below each return an **owned** reference, so every one adds a
// reference before returning.
//
// This is the C API's ownership rule and it is not written in the headers.
// CEF's own generated wrapper is the statement of it: client_cpptoc.cc ends
// get_render_handler with `return CefRenderHandlerCppToC_Wrap(_retval)`, and
// Wrap hands out a reference the caller owns. CEF releases what it is given,
// so a getter that returns a bare pointer lets the count fall below the number
// of outstanding users — the context is freed while CEF is still calling into
// it, and the next callback dereferences freed memory.
//
// It presents as a segmentation fault during the first browser's first frames,
// which says nothing at all about reference counting.

static struct _cef_render_handler_t *CEF_CALLBACK
cl_get_render_handler(struct _cef_client_t *self) {
  ctx_t *c = CTX_OF(client, self);
  ctx_ref(c);
  return &c->render;
}

static struct _cef_life_span_handler_t *CEF_CALLBACK
cl_get_life_span_handler(struct _cef_client_t *self) {
  ctx_t *c = CTX_OF(client, self);
  ctx_ref(c);
  return &c->life;
}

static struct _cef_display_handler_t *CEF_CALLBACK
cl_get_display_handler(struct _cef_client_t *self) {
  ctx_t *c = CTX_OF(client, self);
  ctx_ref(c);
  return &c->display;
}

static struct _cef_load_handler_t *CEF_CALLBACK
cl_get_load_handler(struct _cef_client_t *self) {
  ctx_t *c = CTX_OF(client, self);
  ctx_ref(c);
  return &c->load;
}

static struct _cef_request_handler_t *CEF_CALLBACK
cl_get_request_handler(struct _cef_client_t *self) {
  ctx_t *c = CTX_OF(client, self);
  ctx_ref(c);
  return &c->request;
}

// ---------------------------------------------------------------------------

int gocef_load(const char *dir, int apiVersion) {
  char path[1024];
  snprintf(path, sizeof(path), "%s/libcef.so", dir);

  // RTLD_GLOBAL: CEF's own internals and its subprocesses expect these symbols
  // to be visible process-wide.
  g_lib = dlopen(path, RTLD_NOW | RTLD_GLOBAL);
  if (g_lib == NULL) {
    snprintf(g_err, sizeof(g_err), "cannot load %s: %s", path, dlerror());
    return -1;
  }

  if (resolve((void **)&p_api_hash, "cef_api_hash") != 0 ||
      resolve((void **)&p_api_version, "cef_api_version") != 0 ||
      resolve((void **)&p_initialize, "cef_initialize") != 0 ||
      resolve((void **)&p_execute_process, "cef_execute_process") != 0 ||
      resolve((void **)&p_shutdown, "cef_shutdown") != 0 ||
      resolve((void **)&p_do_message_loop_work, "cef_do_message_loop_work") != 0 ||
      resolve((void **)&p_create_browser, "cef_browser_host_create_browser") != 0 ||
      resolve((void **)&p_utf8_to_utf16, "cef_string_utf8_to_utf16") != 0 ||
      resolve((void **)&p_utf16_to_utf8, "cef_string_utf16_to_utf8") != 0 ||
      resolve((void **)&p_utf8_clear, "cef_string_utf8_clear") != 0 ||
      resolve((void **)&p_userfree_utf16_free,
              "cef_string_userfree_utf16_free") != 0) {
    return -1;
  }

  // The version handshake, before any other CEF call. See shim.h.
  p_api_hash(apiVersion, 0);
  int got = p_api_version();
  if (got != apiVersion) {
    snprintf(g_err, sizeof(g_err),
             "libcef refused API version %d, reports %d", apiVersion, got);
    return -1;
  }
  return 0;
}

int gocef_execute_process(int argc, char **argv) {
  cef_main_args_t args;
  args.argc = argc;
  args.argv = argv;
  return p_execute_process(&args, NULL, NULL);
}

int gocef_initialize(const char *resourcesDir, const char *localesDir,
                     const char *cachePath, const char *rootCachePath,
                     const char *helperPath, const char *switches,
                     int windowlessFrameRate, const char *logFile,
                     int enableSandbox) {
  (void)windowlessFrameRate; // per-browser, applied in gocef_create_browser

  static char *argv[MAX_SWITCHES + 1];
  static char sw[2048];
  int argc = 0;
  argv[argc++] = (char *)"gocef";
  if (switches != NULL && switches[0] != '\0') {
    snprintf(sw, sizeof(sw), "%s", switches);
    char *save = NULL;
    for (char *tok = strtok_r(sw, " ", &save);
         tok != NULL && argc < MAX_SWITCHES; tok = strtok_r(NULL, " ", &save)) {
      argv[argc++] = tok;
    }
  }
  argv[argc] = NULL;

  cef_main_args_t args;
  args.argc = argc;
  args.argv = argv;

  cef_settings_t settings;
  memset(&settings, 0, sizeof(settings));
  settings.size = sizeof(settings);
  // The Go side has already verified the SUID helper and exported
  // CHROME_DEVEL_SANDBOX when it asks for the sandbox; an unverified enable
  // is a process abort, not an error (see shim.h).
  settings.no_sandbox = enableSandbox ? 0 : 1;
  settings.windowless_rendering_enabled = 1;
  settings.log_severity = LOGSEVERITY_WARNING;
  if (logFile != NULL && logFile[0] != '\0') {
    // Chromium's own account of a silent failure: everything the browser,
    // renderer and GPU processes log lands here. VERBOSE, because the failures
    // this exists for are the ones that print nothing at WARNING.
    settings.log_severity = LOGSEVERITY_VERBOSE;
    setstr(&settings.log_file, logFile);
  }
  settings.background_color = 0xFFFFFFFFu;
  setstr(&settings.browser_subprocess_path, helperPath);
  setstr(&settings.resources_dir_path, resourcesDir);
  setstr(&settings.locales_dir_path, localesDir);
  // cache_path is the profile; empty means incognito. root_cache_path holds
  // installation data and the process-singleton lock — CEF defaults it to
  // cache_path, but pass it explicitly so an incognito caller can still keep
  // the singleton lock out of the shared per-user default directory.
  setstr(&settings.cache_path, cachePath);
  setstr(&settings.root_cache_path,
         (rootCachePath != NULL && rootCachePath[0] != '\0') ? rootCachePath
                                                             : cachePath);

  if (p_initialize(&args, &settings, NULL, NULL) != 1) {
    snprintf(g_err, sizeof(g_err), "cef_initialize failed");
    return -1;
  }
  return 0;
}

void gocef_work(void) {
  if (p_do_message_loop_work != NULL) {
    p_do_message_loop_work();
  }
}

void gocef_shutdown(void) {
  if (p_shutdown != NULL) {
    p_shutdown();
  }
}

int gocef_create_browser(uint64_t handle, const char *url, int w, int h,
                         int sharedTexture) {
  int slot = -1;
  for (int i = 0; i < MAX_BROWSERS; i++) {
    if (g_ctx[i] == NULL) {
      slot = i;
      break;
    }
  }
  if (slot < 0) {
    snprintf(g_err, sizeof(g_err), "too many browsers (max %d)", MAX_BROWSERS);
    return -1;
  }

  ctx_t *c = calloc(1, sizeof(ctx_t));
  if (c == NULL) {
    snprintf(g_err, sizeof(g_err), "out of memory");
    return -1;
  }
  c->handle = handle;
  // One reference for the shim itself, released by gocef_close_browser. A
  // second is added below for the client passed into create_browser, because
  // arguments are transferred the same way return values are: CEF takes
  // ownership of what it is handed and releases it when done.
  atomic_init(&c->refs, 1);

  init_base(&c->client.base, sizeof(c->client), client_add_ref, client_release,
            client_has_one, client_has_any);
  c->client.get_render_handler = cl_get_render_handler;
  c->client.get_life_span_handler = cl_get_life_span_handler;
  c->client.get_display_handler = cl_get_display_handler;
  c->client.get_load_handler = cl_get_load_handler;
  c->client.get_request_handler = cl_get_request_handler;

  init_base(&c->render.base, sizeof(c->render), render_add_ref, render_release,
            render_has_one, render_has_any);
  c->render.get_view_rect = rh_get_view_rect;
  c->render.get_screen_info = rh_get_screen_info;
  c->render.on_paint = rh_on_paint;
  c->render.on_accelerated_paint = rh_on_accelerated_paint;
  c->render.on_popup_show = rh_on_popup_show;
  c->render.on_popup_size = rh_on_popup_size;

  init_base(&c->life.base, sizeof(c->life), life_add_ref, life_release,
            life_has_one, life_has_any);
  c->life.on_before_popup = ls_on_before_popup;
  c->life.on_after_created = ls_on_after_created;
  c->life.on_before_close = ls_on_before_close;

  init_base(&c->display.base, sizeof(c->display), display_add_ref,
            display_release, display_has_one, display_has_any);
  c->display.on_cursor_change = dh_on_cursor_change;

  init_base(&c->load.base, sizeof(c->load), load_add_ref, load_release,
            load_has_one, load_has_any);
  c->load.on_loading_state_change = lh_on_loading_state_change;
  c->load.on_load_error = lh_on_load_error;

  init_base(&c->request.base, sizeof(c->request), request_add_ref,
            request_release, request_has_one, request_has_any);
  c->request.on_before_browse = rq_on_before_browse;

  init_base(&c->devtools.base, sizeof(c->devtools), devtools_add_ref,
            devtools_release, devtools_has_one, devtools_has_any);
  c->devtools.on_dev_tools_message = dt_on_message;

  g_ctx[slot] = c;

  cef_window_info_t wi;
  memset(&wi, 0, sizeof(wi));
  wi.size = sizeof(wi);
  wi.windowless_rendering_enabled = 1;
  wi.shared_texture_enabled = sharedTexture ? 1 : 0;
  wi.bounds.width = w;
  wi.bounds.height = h;

  cef_browser_settings_t bs;
  memset(&bs, 0, sizeof(bs));
  bs.size = sizeof(bs);
  // 60 rather than CEF's default of 30: bellui repaints at display rate, and a
  // half-rate web view reads as stutter.
  bs.windowless_frame_rate = 60;

  cef_string_t curl;
  memset(&curl, 0, sizeof(curl));
  setstr(&curl, url);

  // The reference transferred to CEF with the client.
  ctx_ref(c);

  if (p_create_browser(&wi, &c->client, &curl, &bs, NULL, NULL) != 1) {
    snprintf(g_err, sizeof(g_err), "cef_browser_host_create_browser failed");
    ctx_unref(c); // the transfer CEF did not take
    ctx_unref(c); // the shim's own reference
    return -1;
  }
  return 0;
}

// host_of returns the browser host for a handle, with a reference the caller
// must release.
static cef_browser_host_t *host_of(uint64_t handle) {
  ctx_t *c = ctx_find(handle);
  if (c == NULL || c->browser == NULL) {
    return NULL;
  }
  return c->browser->get_host(c->browser);
}

void gocef_close_browser(uint64_t handle) {
  cef_browser_host_t *host = host_of(handle);
  if (host != NULL) {
    host->close_browser(host, 1);
    host->base.release(&host->base);
  }
  ctx_t *c = ctx_find(handle);
  if (c != NULL) {
    ctx_unref(c); // drop the shim's own reference
  }
}

void gocef_resize(uint64_t handle, int w, int h) {
  (void)w;
  (void)h;
  cef_browser_host_t *host = host_of(handle);
  if (host != NULL) {
    // CEF re-reads the size through get_view_rect, which the Go side answers.
    host->was_resized(host);
    host->base.release(&host->base);
  }
}

void gocef_set_visible(uint64_t handle, int visible) {
  cef_browser_host_t *host = host_of(handle);
  if (host != NULL) {
    host->was_hidden(host, visible ? 0 : 1);
    host->base.release(&host->base);
  }
}

void gocef_set_focus(uint64_t handle, int focused) {
  cef_browser_host_t *host = host_of(handle);
  if (host != NULL) {
    host->set_focus(host, focused ? 1 : 0);
    host->base.release(&host->base);
  }
}

void gocef_invalidate(uint64_t handle) {
  cef_browser_host_t *host = host_of(handle);
  if (host != NULL) {
    host->invalidate(host, PET_VIEW);
    host->base.release(&host->base);
  }
}

void gocef_load_url(uint64_t handle, const char *url) {
  ctx_t *c = ctx_find(handle);
  if (c == NULL || c->browser == NULL) {
    return;
  }
  cef_frame_t *frame = c->browser->get_main_frame(c->browser);
  if (frame == NULL) {
    return;
  }
  cef_string_t curl;
  memset(&curl, 0, sizeof(curl));
  setstr(&curl, url);
  frame->load_url(frame, &curl);
  frame->base.release(&frame->base);
}

// ---------------------------------------------------------------------------
// Navigation

int gocef_nav(uint64_t handle, int op) {
  ctx_t *c = ctx_find(handle);
  if (c == NULL || c->browser == NULL) {
    return 0;
  }
  cef_browser_t *b = c->browser;
  switch (op) {
    case GOCEF_NAV_CAN_GO_BACK:
      return b->can_go_back(b);
    case GOCEF_NAV_GO_BACK:
      b->go_back(b);
      return 1;
    case GOCEF_NAV_CAN_GO_FORWARD:
      return b->can_go_forward(b);
    case GOCEF_NAV_GO_FORWARD:
      b->go_forward(b);
      return 1;
    case GOCEF_NAV_RELOAD:
      b->reload(b);
      return 1;
    case GOCEF_NAV_RELOAD_NO_CACHE:
      b->reload_ignore_cache(b);
      return 1;
    case GOCEF_NAV_STOP:
      b->stop_load(b);
      return 1;
    default:
      return 0;
  }
}

// ---------------------------------------------------------------------------
// Input

void gocef_send_mouse_move(uint64_t handle, int x, int y, uint32_t modifiers,
                           int leave) {
  cef_browser_host_t *host = host_of(handle);
  if (host == NULL) {
    return;
  }
  cef_mouse_event_t ev;
  ev.x = x;
  ev.y = y;
  ev.modifiers = modifiers;
  host->send_mouse_move_event(host, &ev, leave);
  host->base.release(&host->base);
}

void gocef_send_mouse_click(uint64_t handle, int x, int y, uint32_t modifiers,
                            int button, int mouseUp, int clickCount) {
  cef_browser_host_t *host = host_of(handle);
  if (host == NULL) {
    return;
  }
  cef_mouse_event_t ev;
  ev.x = x;
  ev.y = y;
  ev.modifiers = modifiers;
  host->send_mouse_click_event(host, &ev, (cef_mouse_button_type_t)button,
                               mouseUp, clickCount);
  host->base.release(&host->base);
}

void gocef_send_mouse_wheel(uint64_t handle, int x, int y, uint32_t modifiers,
                            int deltaX, int deltaY) {
  cef_browser_host_t *host = host_of(handle);
  if (host == NULL) {
    return;
  }
  cef_mouse_event_t ev;
  ev.x = x;
  ev.y = y;
  ev.modifiers = modifiers;
  host->send_mouse_wheel_event(host, &ev, deltaX, deltaY);
  host->base.release(&host->base);
}

void gocef_send_key(uint64_t handle, int keyType, uint32_t modifiers,
                    int windowsKeyCode, int nativeKeyCode, uint16_t character,
                    uint16_t unmodifiedCharacter, int isSystemKey) {
  cef_browser_host_t *host = host_of(handle);
  if (host == NULL) {
    return;
  }
  cef_key_event_t ev;
  memset(&ev, 0, sizeof(ev));
  ev.size = sizeof(ev);
  ev.type = (cef_key_event_type_t)keyType;
  ev.modifiers = modifiers;
  ev.windows_key_code = windowsKeyCode;
  ev.native_key_code = nativeKeyCode;
  ev.character = (char16_t)character;
  ev.unmodified_character = (char16_t)unmodifiedCharacter;
  ev.is_system_key = isSystemKey;
  host->send_key_event(host, &ev);
  host->base.release(&host->base);
}

// ---------------------------------------------------------------------------
// DevTools protocol

int gocef_send_devtools(uint64_t handle, const void *msg, size_t n) {
  cef_browser_host_t *host = host_of(handle);
  if (host == NULL) {
    return 0;
  }
  int ok = host->send_dev_tools_message(host, msg, n);
  host->base.release(&host->base);
  return ok;
}
