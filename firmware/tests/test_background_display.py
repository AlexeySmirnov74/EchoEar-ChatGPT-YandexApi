#!/usr/bin/env python3
"""Exercise the actual display state methods with host graphics mocks."""
from pathlib import Path
import subprocess
import tempfile

source = (Path(__file__).parents[1] / 'main/display/emote_display.cc').read_text(encoding='utf-8')
methods = source[source.index('bool EmoteDisplay::ShowExperimentalImage'):source.index('void EmoteDisplay::HandleImagePreview')]
harness = r'''
#include <atomic>
#include <cassert>
#include <climits>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <string>
#define ESP_LOGI(...) ((void)0)
#define C_ARRAY_HEADER_MAGIC 1
#define GFX_COLOR_FORMAT_RGB565 2
#define ESP_OK 0
static constexpr size_t kPreviewWidth=240, kPreviewBytes=115200;
struct Image { struct { int magic,cf,w,h,stride; } header{}; size_t data_size=0; const uint8_t* data=nullptr; };
static Image g_preview_descriptor{};
static std::shared_ptr<uint8_t> g_preview_pixels, g_background_pixels;
static uint32_t g_preview_id=0, g_background_id=0, g_preview_epoch=0, g_background_epoch=0;
static std::atomic<int64_t> g_preview_deadline_us{0};
static void* g_obj_preview=(void*)1; static void* g_obj_anim_eye=(void*)2; static void* g_preview_timer=(void*)3;
static bool preview_visible=false, face_visible=true;
static bool fail_next_source=false;
static int64_t now=1;
static int64_t esp_timer_get_time(){return now;}
static void gfx_obj_set_visible(void* obj, bool visible) { if(obj==g_obj_preview)preview_visible=visible; else if(obj==g_obj_anim_eye)face_visible=visible; }
static int gfx_img_set_src(void*,const Image* d) {if(fail_next_source){fail_next_source=false;return 1;} return d->data ? ESP_OK : 1;}
class DisplayLockGuard {public: explicit DisplayLockGuard(void*) {}};
static std::string ack;
class Application { public: uint32_t epoch=1; static Application& GetInstance(){static Application a;return a;}
  bool IsImageEpochCurrent(uint32_t e) {return e==epoch;}
  void SendImageBackgroundAck(const char* op,uint32_t id,const char* status,const char*,uint32_t e) {if(e==epoch)ack=std::string(op)+":"+std::to_string(id)+":"+status;}
};
class EmoteDisplay { public: void* engine_=(void*)1;
  bool ShowExperimentalImage(std::shared_ptr<uint8_t>,int,uint32_t,uint32_t epoch=1);
  void ExpireExperimentalImage(); void PinExperimentalImage(uint32_t,uint32_t epoch=1); void ClearExperimentalImage(uint32_t,uint32_t epoch=1);
};
''' + methods + r'''
int main() {
 EmoteDisplay d;
 auto first=std::shared_ptr<uint8_t>((uint8_t*)malloc(kPreviewBytes),free);
 auto second=std::shared_ptr<uint8_t>((uint8_t*)malloc(kPreviewBytes),free);
 assert(d.ShowExperimentalImage(first,30000,1)); assert(preview_visible && !face_visible);
 d.PinExperimentalImage(2); assert(ack=="pin:2:error" && !g_background_pixels);
 d.PinExperimentalImage(1); assert(ack=="pin:1:ok" && g_background_pixels==first);
 fail_next_source=true; assert(!d.ShowExperimentalImage(second,30000,2));
 assert(g_preview_pixels==first && g_background_pixels==first && preview_visible);
 assert(d.ShowExperimentalImage(second,30000,2)); assert(g_background_pixels==first && g_preview_pixels==second);
 d.PinExperimentalImage(1); assert(ack=="pin:1:error" && g_background_pixels==first);
 now += 30000001; d.PinExperimentalImage(2); assert(ack=="pin:2:error");
 d.ExpireExperimentalImage(); assert(g_preview_pixels==first && g_preview_id==1 && preview_visible && ack=="show:1:ok");
 d.PinExperimentalImage(1); assert(ack=="pin:1:ok");
 assert(d.ShowExperimentalImage(second,30000,3)); d.PinExperimentalImage(3);
 assert(g_background_pixels==second && ack=="pin:3:ok");
 d.ClearExperimentalImage(9); assert(ack=="clear:9:ok" && face_visible && !preview_visible);
 assert(!g_background_pixels && !g_preview_pixels && g_preview_deadline_us==0);
 d.PinExperimentalImage(3); assert(ack=="pin:3:error");
 assert(d.ShowExperimentalImage(first,30000,4)); now+=30000001; d.ExpireExperimentalImage();
 assert(face_visible && !preview_visible && !g_preview_pixels && ack=="hide:4:ok");
 assert(d.ShowExperimentalImage(first,30000,5)); auto previous=ack;
 Application::GetInstance().epoch=2; d.PinExperimentalImage(5,1); d.ClearExperimentalImage(6,1);
 assert(ack==previous && g_preview_pixels==first);
 d.engine_=nullptr; d.ClearExperimentalImage(7,2); assert(ack=="clear:7:error");
}
'''
with tempfile.TemporaryDirectory(prefix='echoear-bg-test-') as tmp:
    cpp = Path(tmp) / 'test.cc'
    cpp.write_text(harness, encoding='utf-8')
    exe = Path(tmp) / 'test'
    subprocess.run(['g++', '-std=c++17', '-Wall', '-Wextra', '-Werror', str(cpp), '-o', str(exe)], check=True)
    subprocess.run([str(exe)], check=True)
print('Actual display methods: pin, overlay, stale pin, expiry, restore, replace and clear passed.')
