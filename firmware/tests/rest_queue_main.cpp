int main() {
 AudioService s;
 assert(s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>()));
 std::atomic_bool completed{false};
 bool accepted=true;
 std::thread producer([&] {
   accepted=s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>(), true, false, true, 0);
   completed=true;
 });
 std::this_thread::sleep_for(std::chrono::milliseconds(50));
 assert(!completed);
 { std::lock_guard<std::mutex> guard(s.audio_queue_mutex_); ++s.playback_generation_; s.audio_decode_queue_.clear(); }
 s.audio_queue_cv_.notify_all();
 producer.join();
 assert(completed && !accepted && s.audio_decode_queue_.empty());
 assert(!s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>(), false, false, true, 0));
 assert(s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>(), false, false, true, 1));
 assert(!s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>()));
 { std::lock_guard<std::mutex> guard(s.audio_queue_mutex_); s.audio_decode_queue_.clear(); s.remote_audio_blocked_=true; }
 assert(!s.PushPacketToDecodeQueue(std::make_unique<AudioStreamPacket>(), false, true));
 std::puts("guarded audio enqueue cancellation passed");
}
