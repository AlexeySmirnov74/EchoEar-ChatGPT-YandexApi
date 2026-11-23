#!/usr/bin/env python3
"""Compile and exercise the actual preview receiver with host-side ESP mocks."""
from pathlib import Path
import subprocess
import tempfile

source = (Path(__file__).parents[1] / 'main/display/emote_display.cc').read_text(encoding='utf-8')
policy = source[source.index('static constexpr size_t kPreviewWidth'):source.index('static void PreviewTimer')]
receiver = source[source.index('void EmoteDisplay::HandleImagePreview'):source.index('void EmoteDisplay::SetTheme')]

harness = r'''
#include <algorithm>
#include <array>
#include <atomic>
#include <cassert>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <vector>
#include <openssl/sha.h>
#include <openssl/evp.h>
#define ESP_LOGI(...) ((void)0)
#define ESP_LOGW(...) ((void)0)
#define MALLOC_CAP_SPIRAM 1
#define MALLOC_CAP_8BIT 2
static void* g_preview_timer = reinterpret_cast<void*>(1);
static std::atomic<bool> g_preview_publish_pending{false};
struct cJSON {
  std::map<std::string,cJSON> fields;
  std::string str;
  const char* valuestring = nullptr;
  double valuedouble = 0;
  int kind = 0;
};
static const cJSON* cJSON_GetObjectItemCaseSensitive(const cJSON* obj,const char* name) {
  auto i=obj->fields.find(name); if(i==obj->fields.end()) return nullptr;
  auto* item=const_cast<cJSON*>(&i->second); item->valuestring=item->str.c_str(); return item;
}
static bool cJSON_IsString(const cJSON* o) { return o && o->kind==1; }
static bool cJSON_IsNumber(const cJSON* o) { return o && o->kind==2; }
static int64_t clock_us=0;
static int64_t esp_timer_get_time() { return clock_us; }
static void* heap_caps_malloc(size_t n,int) { return malloc(n); }
[[maybe_unused]] static unsigned uxTaskGetStackHighWaterMark(void*) { return 999; }
static int mbedtls_sha256(const uint8_t* p,size_t n,uint8_t* out,int) { SHA256(p,n,out); return 0; }
static int mbedtls_base64_decode(uint8_t* out,size_t cap,size_t* length,const uint8_t* in,size_t n) {
  if (!n || n%4) return -1;
  std::vector<unsigned char> tmp(n);
  int z=EVP_DecodeBlock(tmp.data(),in,n); if (z<0) return -1;
  if(in[n-1]=='=') --z; if(in[n-2]=='=') --z;
  if (static_cast<size_t>(z)>cap) return -1;
  std::memcpy(out,tmp.data(),z); *length=z; return 0;
}
static std::vector<std::string> acks;
class Application { public: bool defer=false; uint32_t epoch=1; std::vector<std::function<void()>> tasks;
  static Application& GetInstance(){static Application a;return a;} void Schedule(std::function<void()> f){if(defer)tasks.push_back(std::move(f));else f();}
  void Flush(){auto copy=std::move(tasks);tasks.clear();for(auto& f:copy)f();}
  bool IsImageEpochCurrent(uint32_t e) {return e==epoch;}
  void SendImageBackgroundAck(const char* op,uint32_t,const char* status,const char*,uint32_t e) {if(e==epoch)acks.push_back(std::string(op)+":"+status);}
};
static int shown=0;
class EmoteDisplay { public:
  void HandleImagePreview(const cJSON*,uint32_t epoch=1);
  bool ShowExperimentalImage(std::shared_ptr<uint8_t> p,int duration,uint32_t id,uint32_t) { assert(p); assert(duration==15000); assert(id==1); ++shown; return true; }
  void PinExperimentalImage(uint32_t,uint32_t e) { if(Application::GetInstance().IsImageEpochCurrent(e))acks.push_back("pin:called"); }
  void ClearExperimentalImage(uint32_t,uint32_t e) { if(Application::GetInstance().IsImageEpochCurrent(e))acks.push_back("clear:called"); }
};
''' + policy + receiver + r'''
static void str(cJSON& o,const char* k,std::string v) { auto& f=o.fields[k]; f.str=v; f.kind=1; }
static void num(cJSON& o,const char* k,double v) { auto& f=o.fields[k]; f.valuedouble=v; f.kind=2; }
static std::string base64(const uint8_t* p,size_t n) { std::string out((n+2)/3*4,'\0'); EVP_EncodeBlock(reinterpret_cast<unsigned char*>(&out[0]),p,n); return out; }
static std::string shahex(const std::vector<uint8_t>& data) {
  uint8_t hash[32]; SHA256(data.data(),data.size(),hash); const char* hex="0123456789abcdef"; std::string out;
  for (auto b:hash) { out+=hex[b>>4];out+=hex[b&15]; } return out;
}
static cJSON make_begin(const std::vector<uint8_t>& data) { cJSON r; str(r,"op","begin");num(r,"id",1);num(r,"width",240);num(r,"height",240);num(r,"bytes",115200);num(r,"duration_ms",15000);str(r,"sha256",shahex(data));return r; }
static cJSON chunk(int seq,const uint8_t* p,size_t n) { cJSON r;str(r,"op","chunk");num(r,"id",1);num(r,"seq",seq);str(r,"data",base64(p,n));return r; }
static cJSON end() { cJSON r;str(r,"op","end");num(r,"id",1);return r; }
static void send(EmoteDisplay& d,const std::vector<uint8_t>& data) {
  auto b=make_begin(data);d.HandleImagePreview(&b);int seq=0;
  for(size_t off=0;off<data.size();off+=1024) { auto c=chunk(seq++,data.data()+off,std::min(size_t(1024),data.size()-off));d.HandleImagePreview(&c); }
  auto e=end();d.HandleImagePreview(&e);
}
int main() {
  EmoteDisplay d;std::vector<uint8_t> data(115200);for(size_t i=0;i<data.size();i++) data[i]=i%251;
  send(d,data);assert(shown==1 && !g_transfer.data && acks.back()=="show:ok");
  auto b=make_begin(data);d.HandleImagePreview(&b);auto wrong=chunk(1,data.data(),1024);d.HandleImagePreview(&wrong);assert(!g_transfer.data);
  b=make_begin(data);num(b,"width",241);d.HandleImagePreview(&b);assert(!g_transfer.data);
  b=make_begin(data);num(b,"id",1.25);d.HandleImagePreview(&b);assert(!g_transfer.data);
  b=make_begin(data);d.HandleImagePreview(&b);auto too_big=chunk(0,data.data(),1025);d.HandleImagePreview(&too_big);assert(!g_transfer.data);
  b=make_begin(data);d.HandleImagePreview(&b);auto short_end=end();d.HandleImagePreview(&short_end);assert(!g_transfer.data && shown==1);
  b=make_begin(data);d.HandleImagePreview(&b);clock_us+=15000001;auto late=chunk(0,data.data(),1024);d.HandleImagePreview(&late);assert(!g_transfer.data);
  b=make_begin(data);d.HandleImagePreview(&b);int seq=0;
  for(size_t off=0;off<data.size();off+=1024) {auto c=chunk(seq++,data.data()+off,std::min(size_t(1024),data.size()-off)); if(off==0)str(c,"data",base64(data.data()+1,1024)); d.HandleImagePreview(&c);}
  auto e=end();d.HandleImagePreview(&e);assert(shown==1 && !g_transfer.data && acks.back()=="show:error");
  b=make_begin(data);d.HandleImagePreview(&b); assert(g_transfer.data);
  cJSON clear;str(clear,"op","clear");num(clear,"id",9);d.HandleImagePreview(&clear);assert(!g_transfer.data && acks.back()=="clear:called");
  cJSON pin;str(pin,"op","pin");num(pin,"id",1);d.HandleImagePreview(&pin);assert(acks.back()=="pin:called");
  send(d,data);assert(shown==2);
  auto& app=Application::GetInstance(); app.defer=true;
  send(d,data); assert(shown==2 && g_preview_publish_pending);
  d.HandleImagePreview(&clear); assert(shown==2);
  app.Flush(); assert(shown==3 && !g_preview_publish_pending);
  assert(acks[acks.size()-2]=="show:ok" && acks.back()=="clear:called");
  send(d,data); assert(g_preview_publish_pending); auto previous=acks.size();
  app.epoch=2; app.Flush(); assert(shown==3 && acks.size()==previous && !g_preview_publish_pending);
  app.defer=false; d.HandleImagePreview(&b,1);assert(!g_transfer.data);
  d.HandleImagePreview(&b,2);assert(g_transfer.data && g_transfer.epoch==2);
  return 0;
}
'''
with tempfile.TemporaryDirectory(prefix='echoear-image-test-') as tmp:
    cpp = Path(tmp) / 'test.cc'
    cpp.write_text(harness, encoding='utf-8')
    executable = Path(tmp) / 'test'
    subprocess.run(['g++', '-std=c++17', '-Wall', '-Wextra', '-Werror', '-Wno-misleading-indentation', str(cpp), '-lcrypto', '-o', str(executable)], check=True)
    subprocess.run([str(executable)], check=True)
print('Actual preview receiver: valid transfer, bounds, sequence, timeout, integrity and retry passed.')
