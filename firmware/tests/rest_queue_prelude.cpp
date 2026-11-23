#include <atomic>
#include <cassert>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <deque>
#include <memory>
#include <mutex>
#include <thread>
constexpr size_t MAX_DECODE_PACKETS_IN_QUEUE = 1;
struct AudioStreamPacket {};
class AudioService {
public:
  bool PushPacketToDecodeQueue(std::unique_ptr<AudioStreamPacket> packet, bool wait = false, bool remote = false,
                               bool guard_generation = false, uint32_t expected_generation = 0);
  std::mutex audio_queue_mutex_;
  std::condition_variable audio_queue_cv_;
  std::deque<std::unique_ptr<AudioStreamPacket>> audio_decode_queue_;
  std::atomic<uint32_t> playback_generation_{0};
  bool remote_audio_blocked_ = false;
};
