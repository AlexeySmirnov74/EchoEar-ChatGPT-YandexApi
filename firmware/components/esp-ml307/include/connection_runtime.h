#pragma once

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <functional>
#include <memory>
#include <mutex>
#include <thread>
#include <vector>

// Proof-of-concept contract: Interrupt MUST be safe alongside Write/Read and
// MUST wake both. Neither read nor write may use an already destroyed TLS
// context. A real adapter must independently enforce the supplied deadline.
class InterruptibleTransport {
public:
    using Deadline = std::chrono::steady_clock::time_point;
    virtual ~InterruptibleTransport() = default;
    virtual bool Write(const std::vector<uint8_t>& bytes, Deadline deadline) = 0;
    virtual bool Read(Deadline deadline) = 0;
    virtual void Interrupt() = 0;
};

class ConnectionRuntime {
public:
    using EventSink = std::function<void(uint64_t)>;
    static constexpr size_t kMaxFrames = 32;
    static constexpr size_t kMaxBytes = 64 * 1024;

    ConnectionRuntime(std::unique_ptr<InterruptibleTransport> transport, uint64_t epoch,
                      std::chrono::milliseconds write_timeout, EventSink post)
        : transport_(std::move(transport)), epoch_(epoch), write_timeout_(write_timeout), post_(std::move(post)),
          writer_([this] { WriteLoop(); }), reader_([this] { ReadLoop(); }) {}

    ConnectionRuntime(const ConnectionRuntime&) = delete;
    ConnectionRuntime& operator=(const ConnectionRuntime&) = delete;
    ~ConnectionRuntime() { Close(); }

    // Nonblocking with respect to network I/O. False means disconnected or full.
    bool Enqueue(std::vector<uint8_t> frame, bool control = false) {
        std::lock_guard<std::mutex> lock(mu_);
        if (stopping_ || frame.empty() || frame.size() > kMaxBytes || queue_.size() >= kMaxFrames ||
            queued_bytes_ > kMaxBytes - frame.size()) return false;
        queued_bytes_ += frame.size();
        if (control) queue_.push_front(std::move(frame));
        else queue_.push_back(std::move(frame));
        cv_.notify_one();
        return true;
    }

    // May run concurrently with enqueue. Serialize multiple close callers;
    // transport/context remains alive until both tasks have joined.
    void Close() {
        std::lock_guard<std::mutex> close_lock(close_mu_);
        {
            std::lock_guard<std::mutex> lock(mu_);
            if (closed_) return;
            stopping_ = true;
            queue_.clear();
            queued_bytes_ = 0;
        }
        cv_.notify_all();
        transport_->Interrupt();
        if (writer_.joinable()) writer_.join();
        if (reader_.joinable()) reader_.join();
        std::lock_guard<std::mutex> lock(mu_);
        closed_ = true;
    }

private:
    void WriteLoop() {
        for (;;) {
            std::vector<uint8_t> frame;
            {
                std::unique_lock<std::mutex> lock(mu_);
                cv_.wait(lock, [this] { return stopping_ || !queue_.empty(); });
                if (stopping_) return;
                frame = std::move(queue_.front());
                queue_.pop_front();
                queued_bytes_ -= frame.size();
            }
            if (!transport_->Write(frame, std::chrono::steady_clock::now() + write_timeout_)) {
                NotifyFailure();
                return;
            }
        }
    }

    void ReadLoop() {
        while (transport_->Read(std::chrono::steady_clock::now() + std::chrono::seconds(1))) {
            std::lock_guard<std::mutex> lock(mu_);
            if (stopping_) return;
        }
        NotifyFailure();
    }

    void NotifyFailure() {
        {
            std::lock_guard<std::mutex> lock(mu_);
            if (stopping_) return;
            stopping_ = true;
            queue_.clear();
            queued_bytes_ = 0;
        }
        cv_.notify_all();
        transport_->Interrupt();
        // Sink must only enqueue a small event. Consumer checks epoch before
        // changing state, because event delivery can lag behind reconnection.
        post_(epoch_);
    }

    std::unique_ptr<InterruptibleTransport> transport_;
    const uint64_t epoch_;
    const std::chrono::milliseconds write_timeout_;
    EventSink post_;
    std::mutex mu_;
    std::mutex close_mu_;
    std::condition_variable cv_;
    std::deque<std::vector<uint8_t>> queue_;
    size_t queued_bytes_ = 0;
    bool stopping_ = false;
    bool closed_ = false;
    std::thread writer_;
    std::thread reader_;
};
