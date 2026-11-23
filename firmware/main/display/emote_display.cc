#include "emote_display.h"

// Standard C++ headers
#include <cstring>
#include <memory>
#include <unordered_map>
#include <tuple>
#include <array>
#include <algorithm>
#include <atomic>
#include <climits>
#include <mutex>
#include <cstdlib>
#include <esp_heap_caps.h>
#include <mbedtls/base64.h>
#include <mbedtls/sha256.h>
#include "application.h"

// Standard C headers
#include <sys/time.h>
#include <time.h>

// ESP-IDF headers
#include <esp_log.h>
#include <esp_lcd_panel_io.h>
#include <esp_timer.h>

// FreeRTOS headers
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

// Project headers
#include "assets.h"
#include "assets/lang_config.h"
#include "board.h"
#include "gfx.h"

LV_FONT_DECLARE(BUILTIN_TEXT_FONT);

namespace emote {

// ============================================================================
// Constants and Type Definitions
// ============================================================================

static const char* TAG = "EmoteDisplay";

// UI Element Names - Centralized Management
#define UI_ELEMENT_EYE_ANIM      "eye_anim"
#define UI_ELEMENT_TOAST_LABEL   "toast_label"
#define UI_ELEMENT_CLOCK_LABEL   "clock_label"
#define UI_ELEMENT_LISTEN_ANIM   "listen_anim"
#define UI_ELEMENT_STATUS_ICON   "status_icon"

// Icon Names - Centralized Management
#define ICON_MIC                 "icon_mic"
#define ICON_BATTERY             "icon_Battery"
#define ICON_SPEAKER_ZZZ         "icon_speaker_zzz"
#define ICON_WIFI_FAILED         "icon_WiFi_failed"
#define ICON_WIFI_OK             "icon_wifi"
#define ICON_LISTEN              "listen"

using FlushIoReadyCallback = std::function<bool(esp_lcd_panel_io_handle_t, esp_lcd_panel_io_event_data_t*, void*)>;
using FlushCallback = std::function<void(gfx_handle_t, int, int, int, int, const void*)>;

// ============================================================================
// Global Variables
// ============================================================================

// UI element management
static gfx_obj_t* g_obj_label_toast = nullptr;
static gfx_obj_t* g_obj_label_clock = nullptr;
static gfx_obj_t* g_obj_anim_eye = nullptr;
static gfx_obj_t* g_obj_anim_listen = nullptr;
static gfx_obj_t* g_obj_img_status = nullptr;
static gfx_obj_t* g_obj_preview = nullptr;
static gfx_image_dsc_t g_preview_descriptor{};
static std::shared_ptr<uint8_t> g_preview_pixels;
static std::shared_ptr<uint8_t> g_background_pixels;
// These fields are mutated only on the Application event loop while holding
// the display lock. A background is separate from the temporary overlay.
static uint32_t g_preview_id = 0;
static uint32_t g_background_id = 0;
static uint32_t g_preview_epoch = 0;
static uint32_t g_background_epoch = 0;
static std::atomic<int64_t> g_preview_deadline_us{0};
static std::atomic<bool> g_preview_expiration_pending{false};
static std::atomic<bool> g_preview_publish_pending{false};
static esp_timer_handle_t g_preview_timer = nullptr;

// This bounded receive state is accessed by the WebSocket reader and timer.
static constexpr size_t kPreviewWidth = 240;
static constexpr size_t kPreviewBytes = kPreviewWidth * kPreviewWidth * 2;
static constexpr size_t kChunkBytes = 1024;
struct PreviewTransfer {
    std::shared_ptr<uint8_t> data;
    uint32_t id = 0;
    uint32_t epoch = 0;
    uint32_t next_seq = 0;
    size_t used = 0;
    int duration_ms = 15000;
    int64_t last_activity_us = 0;
    uint8_t expected_sha[32]{};
};
static std::mutex g_preview_mutex;
static PreviewTransfer g_transfer;

static bool PreviewNumber(const cJSON* object, const char* field, uint32_t* out)
{
    const auto* value = cJSON_GetObjectItemCaseSensitive(object, field);
    if (!cJSON_IsNumber(value) || value->valuedouble < 0 || value->valuedouble > UINT32_MAX) return false;
    auto result = static_cast<uint32_t>(value->valuedouble);
    if (static_cast<double>(result) != value->valuedouble) return false;
    *out = result;
    return true;
}

static bool PreviewSha(const char* text, uint8_t* output)
{
    if (!text || std::strlen(text) != 64) return false;
    auto nibble = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        return -1;
    };
    for (size_t i = 0; i < 32; ++i) {
        int hi = nibble(text[i * 2]), lo = nibble(text[i * 2 + 1]);
        if (hi < 0 || lo < 0) return false;
        output[i] = static_cast<uint8_t>((hi << 4) | lo);
    }
    return true;
}

static void PreviewTimer(void* arg)
{
    const int64_t now = esp_timer_get_time();
    {
        std::lock_guard<std::mutex> lock(g_preview_mutex);
        if (g_transfer.data && now - g_transfer.last_activity_us >= 15000000) {
            g_transfer = {};
            ESP_LOGW(TAG, "Image preview transfer timed out");
        }
    }
    auto deadline = g_preview_deadline_us.load();
    if (deadline && now >= deadline && !g_preview_expiration_pending.exchange(true)) {
        auto* display = static_cast<EmoteDisplay*>(arg);
        Application::GetInstance().Schedule([display]() {
            display->ExpireExperimentalImage();
            g_preview_expiration_pending.store(false);
        });
    }
}

// Track current icon to determine when to show time
static std::string g_current_icon_type = ICON_WIFI_FAILED;
static gfx_image_dsc_t g_icon_img_dsc;


// ============================================================================
// Forward Declarations
// ============================================================================

class EmoteDisplay;
class EmoteEngine;

enum class UIDisplayMode : uint8_t {
    SHOW_LISTENING = 1,  // Show g_obj_anim_listen
    SHOW_TIME = 2,      // Show g_obj_label_clock
    SHOW_TIPS = 3       // Show g_obj_label_toast
};

// ============================================================================
// Helper Functions
// ============================================================================

// Function to convert align string to GFX_ALIGN enum value
char StringToGfxAlign(const std::string &align_str)
{
    static const std::unordered_map<std::string, char> align_map = {
        {"GFX_ALIGN_DEFAULT",           GFX_ALIGN_DEFAULT},
        {"GFX_ALIGN_TOP_LEFT",          GFX_ALIGN_TOP_LEFT},
        {"GFX_ALIGN_TOP_MID",           GFX_ALIGN_TOP_MID},
        {"GFX_ALIGN_TOP_RIGHT",         GFX_ALIGN_TOP_RIGHT},
        {"GFX_ALIGN_LEFT_MID",          GFX_ALIGN_LEFT_MID},
        {"GFX_ALIGN_CENTER",            GFX_ALIGN_CENTER},
        {"GFX_ALIGN_RIGHT_MID",         GFX_ALIGN_RIGHT_MID},
        {"GFX_ALIGN_BOTTOM_LEFT",       GFX_ALIGN_BOTTOM_LEFT},
        {"GFX_ALIGN_BOTTOM_MID",        GFX_ALIGN_BOTTOM_MID},
        {"GFX_ALIGN_BOTTOM_RIGHT",      GFX_ALIGN_BOTTOM_RIGHT},
        {"GFX_ALIGN_OUT_TOP_LEFT",      GFX_ALIGN_OUT_TOP_LEFT},
        {"GFX_ALIGN_OUT_TOP_MID",       GFX_ALIGN_OUT_TOP_MID},
        {"GFX_ALIGN_OUT_TOP_RIGHT",     GFX_ALIGN_OUT_TOP_RIGHT},
        {"GFX_ALIGN_OUT_LEFT_TOP",      GFX_ALIGN_OUT_LEFT_TOP},
        {"GFX_ALIGN_OUT_LEFT_MID",      GFX_ALIGN_OUT_LEFT_MID},
        {"GFX_ALIGN_OUT_LEFT_BOTTOM",   GFX_ALIGN_OUT_LEFT_BOTTOM},
        {"GFX_ALIGN_OUT_RIGHT_TOP",     GFX_ALIGN_OUT_RIGHT_TOP},
        {"GFX_ALIGN_OUT_RIGHT_MID",     GFX_ALIGN_OUT_RIGHT_MID},
        {"GFX_ALIGN_OUT_RIGHT_BOTTOM",  GFX_ALIGN_OUT_RIGHT_BOTTOM},
        {"GFX_ALIGN_OUT_BOTTOM_LEFT",   GFX_ALIGN_OUT_BOTTOM_LEFT},
        {"GFX_ALIGN_OUT_BOTTOM_MID",    GFX_ALIGN_OUT_BOTTOM_MID},
        {"GFX_ALIGN_OUT_BOTTOM_RIGHT",  GFX_ALIGN_OUT_BOTTOM_RIGHT}
    };

    const auto it = align_map.find(align_str);
    if (it != align_map.cend()) {
        return it->second;
    }

    ESP_LOGW(TAG, "Unknown align string: %s, using GFX_ALIGN_DEFAULT", align_str.c_str());
    return GFX_ALIGN_DEFAULT;
}

// ============================================================================
// EmoteEngine Class Declaration
// ============================================================================

class EmoteEngine {
public:
    EmoteEngine(const esp_lcd_panel_handle_t panel, const esp_lcd_panel_io_handle_t panel_io,
                const int width, const int height, EmoteDisplay* const display);
    ~EmoteEngine();

    void SetEyes(const std::string &emoji_name, const bool repeat, const int fps, EmoteDisplay* const display);
    void SetIcon(const std::string &icon_name, EmoteDisplay* const display);

    void* GetEngineHandle() const
    {
        return engine_handle_;
    }

    // Callback functions (public to be accessible from static helper functions)
    static bool OnFlushIoReady(const esp_lcd_panel_io_handle_t panel_io, esp_lcd_panel_io_event_data_t* const edata, void* const user_ctx);
    static void OnFlush(const gfx_handle_t handle, const int x_start, const int y_start, const int x_end, const int y_end, const void* const color_data);

private:
    gfx_handle_t engine_handle_;
};

// ============================================================================
// UI Management Functions
// ============================================================================

static void SetUIDisplayMode(const UIDisplayMode mode, EmoteDisplay* const display)
{
    if (!display) {
        ESP_LOGE(TAG, "SetUIDisplayMode: display is nullptr");
        return;
    }

    gfx_obj_set_visible(g_obj_anim_listen, false);
    gfx_obj_set_visible(g_obj_label_clock, false);
    gfx_obj_set_visible(g_obj_label_toast, false);

    // Show the selected control
    switch (mode) {
    case UIDisplayMode::SHOW_LISTENING: {
        gfx_obj_set_visible(g_obj_anim_listen, true);
        const AssetData emoji_data = display->GetIconData(ICON_LISTEN);
        if (emoji_data.data) {
            gfx_anim_set_src(g_obj_anim_listen, emoji_data.data, emoji_data.size);
            gfx_anim_set_segment(g_obj_anim_listen, 0, 0xFFFF, 20, true);
            gfx_anim_start(g_obj_anim_listen);
        }
        break;
    }
    case UIDisplayMode::SHOW_TIME:
        gfx_obj_set_visible(g_obj_label_clock, true);
        break;
    case UIDisplayMode::SHOW_TIPS:
        gfx_obj_set_visible(g_obj_label_toast, true);
        break;
    }
}

// ============================================================================
// Graphics Initialization Functions
// ============================================================================

static void InitializeGraphics(const esp_lcd_panel_handle_t panel, gfx_handle_t* const engine_handle,
                               const int width, const int height)
{
    if (!panel || !engine_handle) {
        ESP_LOGE(TAG, "InitializeGraphics: Invalid parameters");
        return;
    }

    gfx_core_config_t gfx_cfg = {
        .flush_cb = EmoteEngine::OnFlush,
        .user_data = panel,
        .flags = {
            .swap = true,
            .double_buffer = true,
            .buff_dma = true,
        },
        .h_res = static_cast<uint32_t>(width),
        .v_res = static_cast<uint32_t>(height),
        .fps = 30,
        .buffers = {
            .buf1 = nullptr,
            .buf2 = nullptr,
            .buf_pixels = static_cast<size_t>(width * 16),
        },
        .task = GFX_EMOTE_INIT_CONFIG()
    };

    gfx_cfg.task.task_stack_caps = MALLOC_CAP_DEFAULT;
    gfx_cfg.task.task_affinity = 0;
    gfx_cfg.task.task_priority = 5;
    gfx_cfg.task.task_stack = 8 * 1024;

    *engine_handle = gfx_emote_init(&gfx_cfg);
}

static void SetupUI(const gfx_handle_t engine_handle, EmoteDisplay* const display)
{
    if (!display) {
        ESP_LOGE(TAG, "SetupUI: display is nullptr");
        return;
    }

    gfx_emote_set_bg_color(engine_handle, GFX_COLOR_HEX(0x000000));

    g_obj_anim_eye = gfx_anim_create(engine_handle);
    gfx_obj_align(g_obj_anim_eye, GFX_ALIGN_LEFT_MID, 10, 30);
    gfx_anim_set_auto_mirror(g_obj_anim_eye, true);
    gfx_obj_set_visible(g_obj_anim_eye, false);

    g_obj_label_toast = gfx_label_create(engine_handle);
    gfx_obj_align(g_obj_label_toast, GFX_ALIGN_TOP_MID, 0, 20);
    gfx_obj_set_size(g_obj_label_toast, 200, 40);
    gfx_label_set_text(g_obj_label_toast, Lang::Strings::INITIALIZING);
    gfx_label_set_color(g_obj_label_toast, GFX_COLOR_HEX(0xFFFFFF));
    gfx_label_set_text_align(g_obj_label_toast, GFX_TEXT_ALIGN_CENTER);
    gfx_label_set_long_mode(g_obj_label_toast, GFX_LABEL_LONG_SCROLL);
    gfx_label_set_scroll_speed(g_obj_label_toast, 20);
    gfx_label_set_scroll_loop(g_obj_label_toast, true);
    gfx_label_set_font(g_obj_label_toast, (gfx_font_t)&BUILTIN_TEXT_FONT);

    g_obj_label_clock = gfx_label_create(engine_handle);
    gfx_obj_align(g_obj_label_clock, GFX_ALIGN_TOP_MID, 0, 15);
    gfx_obj_set_size(g_obj_label_clock, 200, 50);
    gfx_label_set_text(g_obj_label_clock, "--:--");
    gfx_label_set_color(g_obj_label_clock, GFX_COLOR_HEX(0xFFFFFF));
    gfx_label_set_text_align(g_obj_label_clock, GFX_TEXT_ALIGN_CENTER);
    gfx_label_set_font(g_obj_label_clock, (gfx_font_t)&BUILTIN_TEXT_FONT);

    g_obj_anim_listen = gfx_anim_create(engine_handle);
    gfx_obj_align(g_obj_anim_listen, GFX_ALIGN_TOP_MID, 0, 5);
    gfx_anim_start(g_obj_anim_listen);
    gfx_obj_set_visible(g_obj_anim_listen, false);

    g_obj_img_status = gfx_img_create(engine_handle);
    gfx_obj_align(g_obj_img_status, GFX_ALIGN_TOP_MID, -120, 18);

    // Created last so the preview covers the face and status elements.
    g_obj_preview = gfx_img_create(engine_handle);
    if (g_obj_preview) {
        gfx_obj_align(g_obj_preview, GFX_ALIGN_CENTER, 0, 4);
        gfx_obj_set_visible(g_obj_preview, false);
    }

    SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, display);
}

static void RegisterCallbacks(const esp_lcd_panel_io_handle_t panel_io, const gfx_handle_t engine_handle)
{
    if (!panel_io) {
        ESP_LOGE(TAG, "RegisterCallbacks: panel_io is nullptr");
        return;
    }

    const esp_lcd_panel_io_callbacks_t cbs = {
        .on_color_trans_done = EmoteEngine::OnFlushIoReady,
    };
    esp_lcd_panel_io_register_event_callbacks(panel_io, &cbs, engine_handle);
}

// ============================================================================
// EmoteEngine Class Implementation
// ============================================================================

EmoteEngine::EmoteEngine(const esp_lcd_panel_handle_t panel, const esp_lcd_panel_io_handle_t panel_io,
                         const int width, const int height, EmoteDisplay* const display)
{
    InitializeGraphics(panel, &engine_handle_, width, height);

    if (display) {
        gfx_emote_lock(engine_handle_);
        SetupUI(engine_handle_, display);
        gfx_emote_unlock(engine_handle_);
    }

    RegisterCallbacks(panel_io, engine_handle_);
}

EmoteEngine::~EmoteEngine()
{
    if (engine_handle_) {
        gfx_emote_deinit(engine_handle_);
        engine_handle_ = nullptr;
    }
}

void EmoteEngine::SetEyes(const std::string &emoji_name, const bool repeat, const int fps, EmoteDisplay* const display)
{
    if (!engine_handle_) {
        ESP_LOGE(TAG, "SetEyes: engine_handle_ is nullptr");
        return;
    }

    if (!display) {
        ESP_LOGE(TAG, "SetEyes: display is nullptr");
        return;
    }

    const AssetData emoji_data = display->GetEmojiData(emoji_name);
    if (emoji_data.data) {
        DisplayLockGuard lock(display);
        gfx_anim_set_src(g_obj_anim_eye, emoji_data.data, emoji_data.size);
        gfx_anim_set_segment(g_obj_anim_eye, 0, 0xFFFF, fps, repeat);
        gfx_obj_set_visible(g_obj_anim_eye, g_preview_deadline_us.load() == 0);
        gfx_anim_start(g_obj_anim_eye);
    } else {
        ESP_LOGW(TAG, "SetEyes: No emoji data found for %s", emoji_name.c_str());
    }
}

void EmoteEngine::SetIcon(const std::string &icon_name, EmoteDisplay* const display)
{
    if (!engine_handle_) {
        ESP_LOGE(TAG, "SetIcon: engine_handle_ is nullptr");
        return;
    }

    if (!display) {
        ESP_LOGE(TAG, "SetIcon: display is nullptr");
        return;
    }

    const AssetData icon_data = display->GetIconData(icon_name);
    if (icon_data.data) {
        DisplayLockGuard lock(display);

        std::memcpy(&g_icon_img_dsc.header, icon_data.data, sizeof(gfx_image_header_t));
        g_icon_img_dsc.data = static_cast<const uint8_t*>(icon_data.data) + sizeof(gfx_image_header_t);
        g_icon_img_dsc.data_size = icon_data.size - sizeof(gfx_image_header_t);

        gfx_img_set_src(g_obj_img_status, &g_icon_img_dsc);
    } else {
        ESP_LOGW(TAG, "SetIcon: No icon data found for %s", icon_name.c_str());
    }
    g_current_icon_type = icon_name;
}

bool EmoteEngine::OnFlushIoReady(const esp_lcd_panel_io_handle_t panel_io,
                                 esp_lcd_panel_io_event_data_t* const edata,
                                 void* const user_ctx)
{
        gfx_handle_t handle = static_cast<gfx_handle_t>(user_ctx);
    if (handle) {
        gfx_emote_flush_ready(handle, true);
    }
    return false;
}

void EmoteEngine::OnFlush(const gfx_handle_t handle, const int x_start, const int y_start,
                          const int x_end, const int y_end, const void* const color_data)
{
    auto* const panel = static_cast<esp_lcd_panel_handle_t>(gfx_emote_get_user_data(handle));
    if (panel) {
        esp_lcd_panel_draw_bitmap(panel, x_start, y_start, x_end, y_end, color_data);
    }
}

// ============================================================================
// EmoteDisplay Class Implementation
// ============================================================================

EmoteDisplay::EmoteDisplay(const esp_lcd_panel_handle_t panel, const esp_lcd_panel_io_handle_t panel_io,
                           const int width, const int height)
{
    width_ = width;
    height_ = height;
    InitializeEngine(panel, panel_io, width, height);
    const esp_timer_create_args_t timer_args = {
        .callback = PreviewTimer,
        .arg = this,
        .dispatch_method = ESP_TIMER_TASK,
        .name = "image_preview",
        .skip_unhandled_events = true,
    };
    if (g_obj_preview && esp_timer_create(&timer_args, &g_preview_timer) == ESP_OK) {
        if (esp_timer_start_periodic(g_preview_timer, 1000000) != ESP_OK) {
            esp_timer_delete(g_preview_timer);
            g_preview_timer = nullptr;
        }
    }
}

EmoteDisplay::~EmoteDisplay()
{
    if (g_preview_timer) {
        esp_timer_stop(g_preview_timer);
        esp_timer_delete(g_preview_timer);
        g_preview_timer = nullptr;
    }
}

void EmoteDisplay::SetEmotion(const char* const emotion)
{
    if (!emotion) {
        ESP_LOGE(TAG, "SetEmotion: emotion is nullptr");
        return;
    }

    ESP_LOGI(TAG, "SetEmotion: %s", emotion);
    if (!engine_) {
        return;
    }

    const AssetData emoji_data = GetEmojiData(emotion);
    bool repeat = emoji_data.loop;
    int fps = emoji_data.fps > 0 ? emoji_data.fps : 20;

    if (std::strcmp(emotion, "idle") == 0 || std::strcmp(emotion, "neutral") == 0) {
        repeat = false;
    }

    DisplayLockGuard lock(this);
    engine_->SetEyes(emotion, repeat, fps, this);
}

void EmoteDisplay::SetChatMessage(const char* const role, const char* const content)
{
    if (!engine_) {
        return;
    }

    DisplayLockGuard lock(this);
    if (content && strlen(content) > 0) {
        gfx_label_set_text(g_obj_label_toast, content);
        SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, this);
    }
}

void EmoteDisplay::SetStatus(const char* const status)
{
    if (!status) {
        ESP_LOGE(TAG, "SetStatus: status is nullptr");
        return;
    }

    if (!engine_) {
        return;
    }

    DisplayLockGuard lock(this);

    if (std::strcmp(status, Lang::Strings::LISTENING) == 0) {
        SetUIDisplayMode(UIDisplayMode::SHOW_LISTENING, this);
        engine_->SetEyes("happy", true, 20, this);
        engine_->SetIcon(ICON_MIC, this);
    } else if (std::strcmp(status, Lang::Strings::STANDBY) == 0) {
        SetUIDisplayMode(UIDisplayMode::SHOW_TIME, this);
        engine_->SetIcon(ICON_BATTERY, this);
    } else if (std::strcmp(status, Lang::Strings::SPEAKING) == 0) {
        SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, this);
        engine_->SetIcon(ICON_SPEAKER_ZZZ, this);
    } else if (std::strcmp(status, Lang::Strings::ERROR) == 0) {
        SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, this);
        engine_->SetIcon(ICON_WIFI_FAILED, this);
    }

    if (std::strcmp(status, Lang::Strings::CONNECTING) != 0) {
        gfx_label_set_text(g_obj_label_toast, status);
    }
}

void EmoteDisplay::ShowNotification(const char* notification, int duration_ms)
{
    if (!notification || !engine_) {
        return;
    }
    ESP_LOGI(TAG, "ShowNotification: %s", notification);

    DisplayLockGuard lock(this);
    gfx_label_set_text(g_obj_label_toast, notification);
    SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, this);
}

void EmoteDisplay::UpdateStatusBar(bool update_all)
{
    if (!engine_) {
        return;
    }

    // Only display time when battery icon is shown
    DisplayLockGuard lock(this);
    if (g_current_icon_type == ICON_BATTERY) {
        time_t now;
        struct tm timeinfo;
        time(&now);

        setenv("TZ", "GMT+0", 1);
        tzset();
        localtime_r(&now, &timeinfo);

        char time_str[6];
        snprintf(time_str, sizeof(time_str), "%02d:%02d", timeinfo.tm_hour, timeinfo.tm_min);

        DisplayLockGuard lock(this);
        gfx_label_set_text(g_obj_label_clock, time_str);
        SetUIDisplayMode(UIDisplayMode::SHOW_TIME, this);
    }
}

void EmoteDisplay::SetPowerSaveMode(bool on)
{
    if (!engine_) {
        return;
    }

    DisplayLockGuard lock(this);
    ESP_LOGI(TAG, "SetPowerSaveMode: %s", on ? "ON" : "OFF");
    if (on) {
        gfx_anim_stop(g_obj_anim_eye);
    } else {
        gfx_anim_start(g_obj_anim_eye);
    }
}

void EmoteDisplay::SetPreviewImage(const void* image)
{
    if (image) {
        ESP_LOGI(TAG, "SetPreviewImage: Preview image not supported, using default icon");
        if (engine_) {
        }
    }
}

bool EmoteDisplay::ShowExperimentalImage(std::shared_ptr<uint8_t> pixels, int duration_ms, uint32_t id, uint32_t epoch)
{
    if (!pixels || !engine_ || !g_obj_preview || !g_preview_timer) return false;
    DisplayLockGuard lock(this);
    // Hide before replacing the descriptor and release the old data only with
    // the graphics lock held: the renderer holds a pointer, not a copy.
    auto previous_pixels = g_preview_pixels;
    auto previous_descriptor = g_preview_descriptor;
    auto previous_id = g_preview_id;
    auto previous_epoch = g_preview_epoch;
    auto previous_deadline = g_preview_deadline_us.load();
    gfx_obj_set_visible(g_obj_preview, false);
    g_preview_pixels = std::move(pixels);
    g_preview_descriptor = {};
    g_preview_descriptor.header.magic = C_ARRAY_HEADER_MAGIC;
    g_preview_descriptor.header.cf = GFX_COLOR_FORMAT_RGB565;
    g_preview_descriptor.header.w = kPreviewWidth;
    g_preview_descriptor.header.h = kPreviewWidth;
    g_preview_descriptor.header.stride = kPreviewWidth * 2;
    g_preview_descriptor.data_size = kPreviewBytes;
    g_preview_descriptor.data = g_preview_pixels.get();
    if (gfx_img_set_src(g_obj_preview, &g_preview_descriptor) != ESP_OK) {
        g_preview_pixels = std::move(previous_pixels);
        g_preview_descriptor = previous_descriptor;
        g_preview_id = previous_id;
        g_preview_epoch = previous_epoch;
        if (g_preview_pixels && previous_deadline &&
            gfx_img_set_src(g_obj_preview, &g_preview_descriptor) == ESP_OK) {
            g_preview_deadline_us.store(previous_deadline);
            gfx_obj_set_visible(g_obj_anim_eye, false);
            gfx_obj_set_visible(g_obj_preview, true);
            return false;
        }
        g_preview_pixels.reset();
        g_preview_deadline_us.store(0);
        gfx_obj_set_visible(g_obj_anim_eye, true);
        g_preview_id = 0;
        g_preview_epoch = 0;
        return false;
    }
    g_preview_id = id;
    g_preview_epoch = epoch;
    g_preview_deadline_us.store(esp_timer_get_time() + static_cast<int64_t>(duration_ms) * 1000);
    gfx_obj_set_visible(g_obj_anim_eye, false);
    gfx_obj_set_visible(g_obj_preview, true);
    ESP_LOGI(TAG, "Image preview displayed (%d ms)", duration_ms);
    return true;
}

void EmoteDisplay::ExpireExperimentalImage()
{
    if (!engine_) return;
    uint32_t notify_id = 0, notify_epoch = 0;
    const char* notify_op = "hide";
    {
    DisplayLockGuard lock(this);
    auto deadline = g_preview_deadline_us.load();
    if (!deadline || esp_timer_get_time() < deadline) return;
    notify_id = g_preview_id;
    notify_epoch = g_preview_epoch;
    if (g_background_pixels) {
        // Switch to the retained frame while holding the graphics lock.
        gfx_obj_set_visible(g_obj_preview, false);
        g_preview_pixels = g_background_pixels;
        g_preview_id = g_background_id;
        g_preview_epoch = g_background_epoch;
        g_preview_descriptor.data = g_preview_pixels.get();
        if (gfx_img_set_src(g_obj_preview, &g_preview_descriptor) == ESP_OK) {
            g_preview_deadline_us.store(INT64_MAX);
            gfx_obj_set_visible(g_obj_anim_eye, false);
            gfx_obj_set_visible(g_obj_preview, true);
            ESP_LOGI(TAG, "Image preview expired; background restored");
            notify_op = "show";
            notify_id = g_background_id;
            notify_epoch = g_background_epoch;
        } else {
            g_background_pixels.reset();
            g_background_id = 0;
            g_background_epoch = 0;
        }
    }
    if (std::strcmp(notify_op, "show") != 0) {
    gfx_obj_set_visible(g_obj_preview, false);
    gfx_obj_set_visible(g_obj_anim_eye, true);
    g_preview_deadline_us.store(0);
    g_preview_descriptor.data = nullptr;
    g_preview_pixels.reset();
    g_preview_id = 0;
    g_preview_epoch = 0;
    ESP_LOGI(TAG, "Image preview expired; face restored");
    }
    }
    if (notify_id) Application::GetInstance().SendImageBackgroundAck(notify_op, notify_id, "ok", nullptr, notify_epoch);
}

void EmoteDisplay::PinExperimentalImage(uint32_t id, uint32_t epoch)
{
    if (!Application::GetInstance().IsImageEpochCurrent(epoch)) return;
    bool pinned = false;
    if (engine_ && g_obj_preview) {
        DisplayLockGuard lock(this);
        const auto deadline = g_preview_deadline_us.load();
        // A queued or stale pin must not capture a newer or expired image.
        if (g_preview_pixels && id == g_preview_id && deadline && esp_timer_get_time() < deadline) {
            g_background_pixels = g_preview_pixels;
            g_background_id = id;
            g_background_epoch = epoch;
            g_preview_deadline_us.store(INT64_MAX);
            pinned = true;
        }
    }
    Application::GetInstance().SendImageBackgroundAck("pin", id, pinned ? "ok" : "error", pinned ? nullptr : "no_preview", epoch);
}

void EmoteDisplay::ClearExperimentalImage(uint32_t id, uint32_t epoch)
{
    if (!Application::GetInstance().IsImageEpochCurrent(epoch)) return;
    bool cleared = false;
    if (engine_ && g_obj_preview) {
        DisplayLockGuard lock(this);
        gfx_obj_set_visible(g_obj_preview, false);
        gfx_obj_set_visible(g_obj_anim_eye, true);
        g_preview_deadline_us.store(0);
        g_preview_descriptor.data = nullptr;
        g_preview_pixels.reset();
        g_background_pixels.reset();
        g_preview_id = 0;
        g_background_id = 0;
        g_preview_epoch = 0;
        g_background_epoch = 0;
        cleared = true;
    }
    Application::GetInstance().SendImageBackgroundAck("clear", id, cleared ? "ok" : "error", cleared ? nullptr : "display", epoch);
}

void EmoteDisplay::HandleImagePreview(const cJSON* root, uint32_t epoch)
{
    if (!g_preview_timer || !Application::GetInstance().IsImageEpochCurrent(epoch)) return;
    const auto* op = cJSON_GetObjectItemCaseSensitive(root, "op");
    if (!cJSON_IsString(op)) return;
    uint32_t id = 0;
    if (!PreviewNumber(root, "id", &id) || !id) return;
    std::lock_guard<std::mutex> lock(g_preview_mutex);
    // An old receive callback may have passed the first check just as a new
    // socket took over. Never let it discard the new socket's transfer.
    if (!Application::GetInstance().IsImageEpochCurrent(epoch)) return;
    const int64_t now = esp_timer_get_time();
    if (g_transfer.data && g_transfer.epoch != epoch) g_transfer = {};
    if (std::strcmp(op->valuestring, "pin") == 0) {
        Application::GetInstance().Schedule([this, id, epoch]() { PinExperimentalImage(id, epoch); });
        return;
    }
    if (std::strcmp(op->valuestring, "clear") == 0) {
        g_transfer = {};
        Application::GetInstance().Schedule([this, id, epoch]() { ClearExperimentalImage(id, epoch); });
        return;
    }
    if (std::strcmp(op->valuestring, "begin") == 0) {
        // No more than one shown frame plus one transfer/pending frame.
        if (g_preview_publish_pending.load()) return;
        g_transfer = {};
        uint32_t width, height, size, duration;
        const auto* sha = cJSON_GetObjectItemCaseSensitive(root, "sha256");
        if (!PreviewNumber(root, "width", &width) || width != kPreviewWidth ||
            !PreviewNumber(root, "height", &height) || height != kPreviewWidth ||
            !PreviewNumber(root, "bytes", &size) || size != kPreviewBytes ||
            !PreviewNumber(root, "duration_ms", &duration) || duration < 1000 || duration > 30000 ||
            !cJSON_IsString(sha) || !PreviewSha(sha->valuestring, g_transfer.expected_sha)) {
            g_transfer = {};
            ESP_LOGW(TAG, "Rejected invalid image preview header");
            return;
        }
        auto* buffer = static_cast<uint8_t*>(heap_caps_malloc(kPreviewBytes, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
        if (!buffer) {
            g_transfer = {};
            ESP_LOGW(TAG, "No PSRAM for image preview");
            return;
        }
        g_transfer.data = std::shared_ptr<uint8_t>(buffer, std::free);
        g_transfer.id = id;
        g_transfer.epoch = epoch;
        g_transfer.duration_ms = static_cast<int>(duration);
        g_transfer.last_activity_us = now;
        ESP_LOGI(TAG, "Image preview transfer started; network stack free minimum: %u bytes",
                 static_cast<unsigned>(uxTaskGetStackHighWaterMark(nullptr)));
        return;
    }
    if (!g_transfer.data || g_transfer.id != id || now - g_transfer.last_activity_us >= 15000000) {
        g_transfer = {};
        return;
    }
    if (std::strcmp(op->valuestring, "chunk") == 0) {
        uint32_t seq;
        const auto* data = cJSON_GetObjectItemCaseSensitive(root, "data");
        if (!PreviewNumber(root, "seq", &seq) || seq != g_transfer.next_seq || !cJSON_IsString(data)) {
            g_transfer = {};
            return;
        }
        size_t length = std::strlen(data->valuestring);
        size_t decoded_length = 0;
        // Sample, rather than log every 20 ms frame; no image bytes are logged.
        const bool log_stack = seq < 3 || seq % 16 == 0;
        if (log_stack) {
            ESP_LOGI(TAG, "Image preview chunk %u before decode; network stack free minimum: %u bytes",
                     static_cast<unsigned>(seq), static_cast<unsigned>(uxTaskGetStackHighWaterMark(nullptr)));
        }
        if (length == 0 || length > ((kChunkBytes + 2) / 3) * 4 ||
            mbedtls_base64_decode(g_transfer.data.get() + g_transfer.used,
                                  std::min(kChunkBytes, kPreviewBytes - g_transfer.used), &decoded_length,
                                  reinterpret_cast<const uint8_t*>(data->valuestring), length) != 0 ||
            !decoded_length || decoded_length > kPreviewBytes - g_transfer.used) {
            g_transfer = {};
            ESP_LOGW(TAG, "Image preview chunk rejected");
            return;
        }
        if (log_stack) {
            ESP_LOGI(TAG, "Image preview chunk %u after decode; network stack free minimum: %u bytes",
                     static_cast<unsigned>(seq), static_cast<unsigned>(uxTaskGetStackHighWaterMark(nullptr)));
        }
        g_transfer.used += decoded_length;
        ++g_transfer.next_seq;
        g_transfer.last_activity_us = now;
        return;
    }
    if (std::strcmp(op->valuestring, "end") == 0) {
        if (g_transfer.used != kPreviewBytes) {
            g_transfer = {};
            ESP_LOGW(TAG, "Image preview incomplete");
            return;
        }
        auto frame = std::move(g_transfer.data);
        int duration = g_transfer.duration_ms;
        std::array<uint8_t, 32> expected_sha;
        std::memcpy(expected_sha.data(), g_transfer.expected_sha, expected_sha.size());
        g_transfer = {};
        g_preview_publish_pending.store(true);
        ESP_LOGI(TAG, "Image preview received; network stack free minimum: %u bytes",
                 static_cast<unsigned>(uxTaskGetStackHighWaterMark(nullptr)));
        Application::GetInstance().Schedule([this, frame = std::move(frame), duration, expected_sha, id, epoch]() {
            if (!Application::GetInstance().IsImageEpochCurrent(epoch)) {
                g_preview_publish_pending.store(false);
                return;
            }
            uint8_t actual_sha[32];
            if (mbedtls_sha256(frame.get(), kPreviewBytes, actual_sha, 0) == 0 &&
                std::memcmp(actual_sha, expected_sha.data(), expected_sha.size()) == 0) {
                bool shown = ShowExperimentalImage(frame, duration, id, epoch);
                Application::GetInstance().SendImageBackgroundAck("show", id, shown ? "ok" : "error", shown ? nullptr : "display", epoch);
            } else {
                ESP_LOGW(TAG, "Image preview checksum mismatch");
                Application::GetInstance().SendImageBackgroundAck("show", id, "error", "checksum", epoch);
            }
            g_preview_publish_pending.store(false);
        });
        return;
    }
    g_transfer = {};
}

void EmoteDisplay::SetTheme(Theme* const theme)
{
    ESP_LOGI(TAG, "SetTheme: %p", theme);

}
void EmoteDisplay::AddEmojiData(const std::string &name, const void* const data, const size_t size,
                                uint8_t fps, bool loop, bool lack)
{
    emoji_data_map_[name] = AssetData(data, size, fps, loop, lack);
    ESP_LOGD(TAG, "Added emoji data: %s, size: %d, fps: %d, loop: %s, lack: %s",
             name.c_str(), size, fps, loop ? "true" : "false", lack ? "true" : "false");

    DisplayLockGuard lock(this);
    if (name == "happy") {
        engine_->SetEyes("happy", loop, fps > 0 ? fps : 20, this);
    }
}

void EmoteDisplay::AddIconData(const std::string &name, const void* const data, const size_t size)
{
    icon_data_map_[name] = AssetData(data, size);
    ESP_LOGD(TAG, "Added icon data: %s, size: %d", name.c_str(), size);

    DisplayLockGuard lock(this);
    if (name == ICON_WIFI_FAILED) {
        SetUIDisplayMode(UIDisplayMode::SHOW_TIPS, this);
        engine_->SetIcon(ICON_WIFI_FAILED, this);
    }
}

void EmoteDisplay::AddLayoutData(const std::string &name, const std::string &align_str,
                                 const int x, const int y, const int width, const int height)
{
    const char align_enum = StringToGfxAlign(align_str);
    ESP_LOGI(TAG, "layout: %-12s | %-20s(%d) | %4d, %4d | %4dx%-4d",
             name.c_str(), align_str.c_str(), align_enum, x, y, width, height);

    struct UIElement {
        gfx_obj_t* obj;
        const char* name;
    };

    const UIElement elements[] = {
        {g_obj_anim_eye,     UI_ELEMENT_EYE_ANIM},
        {g_obj_label_toast,  UI_ELEMENT_TOAST_LABEL},
        {g_obj_label_clock,  UI_ELEMENT_CLOCK_LABEL},
        {g_obj_anim_listen,  UI_ELEMENT_LISTEN_ANIM},
        {g_obj_img_status,   UI_ELEMENT_STATUS_ICON}
    };

    DisplayLockGuard lock(this);
    for (const auto &element : elements) {
        if (name == element.name && element.obj) {
            gfx_obj_align(element.obj, align_enum, x, y);
            if (width > 0 && height > 0) {
                gfx_obj_set_size(element.obj, width, height);
            }
            return;
        }
    }

    ESP_LOGW(TAG, "AddLayoutData: UI element '%s' not found", name.c_str());
}

void EmoteDisplay::AddTextFont(std::shared_ptr<LvglFont> text_font)
{
    if (!text_font) {
        ESP_LOGW(TAG, "AddTextFont: text_font is nullptr");
        return;
    }

    text_font_ = text_font;
    ESP_LOGD(TAG, "AddTextFont: Text font added successfully");

    DisplayLockGuard lock(this);
    if (g_obj_label_toast && text_font_) {
        gfx_label_set_font(g_obj_label_toast, const_cast<void*>(static_cast<const void*>(text_font_->font())));
    }
    if (g_obj_label_clock && text_font_) {
        gfx_label_set_font(g_obj_label_clock, const_cast<void*>(static_cast<const void*>(text_font_->font())));
    }
}

AssetData EmoteDisplay::GetEmojiData(const std::string &name) const
{
    const auto it = emoji_data_map_.find(name);
    if (it != emoji_data_map_.cend()) {
        return it->second;
    }
    return AssetData();
}

AssetData EmoteDisplay::GetIconData(const std::string &name) const
{
    const auto it = icon_data_map_.find(name);
    if (it != icon_data_map_.cend()) {
        return it->second;
    }
    return AssetData();
}

EmoteEngine* EmoteDisplay::GetEngine() const
{
    return engine_.get();
}

void* EmoteDisplay::GetEngineHandle() const
{
    return engine_ ? engine_->GetEngineHandle() : nullptr;
}

void EmoteDisplay::InitializeEngine(const esp_lcd_panel_handle_t panel, const esp_lcd_panel_io_handle_t panel_io,
                                    const int width, const int height)
{
    engine_ = std::make_unique<EmoteEngine>(panel, panel_io, width, height, this);
}

bool EmoteDisplay::Lock(const int timeout_ms)
{
    if (engine_ && engine_->GetEngineHandle()) {
        gfx_emote_lock(engine_->GetEngineHandle());
        return true;
    }
    return false;
}

void EmoteDisplay::Unlock()
{
    if (engine_ && engine_->GetEngineHandle()) {
        gfx_emote_unlock(engine_->GetEngineHandle());
    }
}

} // namespace emote
