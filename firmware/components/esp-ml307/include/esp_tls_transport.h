#pragma once

// Isolated draft: the caller MUST join ConnectionRuntime's reader and writer
// before destroying this object. This has not been integrated into ESP-IDF's
// WebSocket stack or validated on lwIP/mbedTLS hardware.
#include "connection_runtime.h"
#include <esp_tls.h>
#include <atomic>
#include <cerrno>
#include <fcntl.h>
#include <sys/select.h>
#include <sys/socket.h>

class EspTlsTransportDraft final : public InterruptibleTransport {
public:
    using DataSink = std::function<void(uint64_t, const uint8_t*, size_t)>;

    // On success takes ownership of an already handshaken TLS object. DataSink
    // must only enqueue bounded work; it must never wait on the UI or Close().
    static std::unique_ptr<EspTlsTransportDraft> Create(esp_tls_t* tls, uint64_t epoch, DataSink sink) {
        int fd = -1;
        if (!tls || !sink || esp_tls_get_conn_sockfd(tls, &fd) != ESP_OK || fd < 0 || fd >= FD_SETSIZE) return nullptr;
        const int flags = fcntl(fd, F_GETFL, 0);
        if (flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) < 0) return nullptr;
        return std::unique_ptr<EspTlsTransportDraft>(new EspTlsTransportDraft(tls, fd, epoch, std::move(sink)));
    }

    ~EspTlsTransportDraft() override { esp_tls_conn_destroy(tls_); }

    bool Write(const std::vector<uint8_t>& bytes, Deadline deadline) override {
        size_t offset = 0;
        while (offset < bytes.size() && !stopping_.load()) {
            if (std::chrono::steady_clock::now() >= deadline) return false;
            int ret;
            {
                // mbedTLS state is shared by read and write. Do not hold this
                // lock across poll; the file descriptor is nonblocking.
                std::lock_guard<std::mutex> lock(tls_mu_);
                if (stopping_.load()) return false;
                ret = esp_tls_conn_write(tls_, bytes.data() + offset, bytes.size() - offset);
            }
            if (ret > 0) {
                offset += static_cast<size_t>(ret);
            } else if (ret == ESP_TLS_ERR_SSL_WANT_READ || ret == ESP_TLS_ERR_SSL_WANT_WRITE) {
                if (Wait(ret == ESP_TLS_ERR_SSL_WANT_READ, deadline) != WaitResult::Ready) return false;
            } else {
                return false;
            }
        }
        return offset == bytes.size();
    }

    bool Read(Deadline deadline) override {
        while (!stopping_.load()) {
            if (std::chrono::steady_clock::now() >= deadline) return true;
            int ret;
            {
                std::lock_guard<std::mutex> lock(tls_mu_);
                if (stopping_.load()) return false;
                ret = esp_tls_conn_read(tls_, receive_bytes_, sizeof(receive_bytes_));
            }
            if (ret > 0) {
                // Not under tls_mu_. The caller must finish this callback before
                // runtime's reader thread can be joined and TLS destroyed.
                sink_(epoch_, receive_bytes_, static_cast<size_t>(ret));
                return !stopping_.load();
            }
            if (ret != ESP_TLS_ERR_SSL_WANT_READ && ret != ESP_TLS_ERR_SSL_WANT_WRITE) return false;
            const auto status = Wait(ret == ESP_TLS_ERR_SSL_WANT_READ, deadline);
            if (status == WaitResult::Timeout) return true;
            if (status == WaitResult::Error) return false;
        }
        return false;
    }

    void Interrupt() override {
        stopping_.store(true);
        ::shutdown(fd_, SHUT_RDWR); // do not free TLS while another task uses it
    }

private:
    enum class WaitResult { Ready, Timeout, Error };
    EspTlsTransportDraft(esp_tls_t* tls, int fd, uint64_t epoch, DataSink sink)
        : tls_(tls), fd_(fd), epoch_(epoch), sink_(std::move(sink)) {}
    WaitResult Wait(bool read, Deadline deadline) {
        while (!stopping_.load()) {
            const auto remaining = deadline - std::chrono::steady_clock::now();
            if (remaining <= Deadline::duration::zero()) return WaitResult::Timeout;
            auto us = std::chrono::duration_cast<std::chrono::microseconds>(remaining).count();
            if (us < 1) us = 1;
            // shutdown() does not reliably wake select() waiting for writable
            // on every stack. Bound the wait so cancellation is rechecked.
            if (us > 50000) us = 50000;
            timeval timeout{static_cast<long>(us / 1000000), static_cast<long>(us % 1000000)};
            fd_set set;
            FD_ZERO(&set);
            FD_SET(fd_, &set);
            const int result = ::select(fd_ + 1, read ? &set : nullptr, read ? nullptr : &set, nullptr, &timeout);
            if (result > 0) return FD_ISSET(fd_, &set) ? WaitResult::Ready : WaitResult::Error;
            if (result == 0) continue;
            if (errno != EINTR) return WaitResult::Error;
        }
        return WaitResult::Error;
    }

    esp_tls_t* const tls_;
    const int fd_;
    const uint64_t epoch_;
    DataSink sink_;
    std::atomic_bool stopping_{false};
    std::mutex tls_mu_;
    // This transport is owned until the reader task has exited. Read is called
    // by that one task only, so this buffer also remains valid throughout the
    // synchronous sink callback without occupying its small stack.
    uint8_t receive_bytes_[2048]{};
};
