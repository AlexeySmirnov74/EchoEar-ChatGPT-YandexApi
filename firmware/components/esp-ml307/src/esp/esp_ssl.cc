#include "esp_ssl.h"
#include <esp_crt_bundle.h>
#include <esp_log.h>
#include <chrono>

static const char* TAG = "EspSsl";

EspSsl::EspSsl() { event_group_ = xEventGroupCreate(); }

EspSsl::~EspSsl() {
    Disconnect();
    if (event_group_ != nullptr) vEventGroupDelete(event_group_);
}

bool EspSsl::Connect(const std::string& host, int port) {
    if (transport_) return false;
    auto* tls = esp_tls_init();
    if (!tls) return false;
    esp_tls_cfg_t cfg = {};
    cfg.crt_bundle_attach = esp_crt_bundle_attach;
    // Handshake is performed by the background connection worker.
    cfg.timeout_ms = 10000;
    if (esp_tls_conn_new_sync(host.c_str(), host.length(), port, &cfg, tls) != 1) {
        last_error_ = -1;
        esp_tls_conn_destroy(tls);
        return false;
    }
    transport_ = EspTlsTransportDraft::Create(tls, 0, [this](uint64_t, const uint8_t* data, size_t size) {
        if (stream_callback_) stream_callback_(std::string(reinterpret_cast<const char*>(data), size));
    });
    if (!transport_) {
        esp_tls_conn_destroy(tls);
        return false;
    }
    connected_ = true;
    xEventGroupClearBits(event_group_, ESP_SSL_EVENT_RECEIVE_TASK_EXIT);
    if (xTaskCreate([](void* arg) {
        auto* ssl = static_cast<EspSsl*>(arg);
        ssl->ReceiveTask();
        xEventGroupSetBits(ssl->event_group_, ESP_SSL_EVENT_RECEIVE_TASK_EXIT);
        vTaskDelete(nullptr);
    }, "ssl_receive", 12288, this, 1, &receive_task_handle_) != pdPASS) {
        connected_ = false;
        transport_->Interrupt();
        transport_.reset();
        return false;
    }
    return true;
}

void EspSsl::Interrupt() {
    connected_ = false;
    if (transport_) transport_->Interrupt();
}

void EspSsl::Disconnect() {
    if (!transport_) return;
    Interrupt();
    // Never free a TLS context while ReceiveTask can still reference it. A
    // blocked application callback delays background teardown rather than UAF.
    xEventGroupWaitBits(event_group_, ESP_SSL_EVENT_RECEIVE_TASK_EXIT, pdFALSE, pdFALSE, portMAX_DELAY);
    receive_task_handle_ = nullptr;
    transport_.reset();
}

int EspSsl::Send(const std::string& data) {
    if (!connected_ || !transport_) return -1;
    const std::vector<uint8_t> bytes(data.begin(), data.end());
    const bool sent = transport_->Write(bytes, std::chrono::steady_clock::now() + std::chrono::seconds(3));
    return sent ? static_cast<int>(data.size()) : -1;
}

void EspSsl::ReceiveTask() {
    while (connected_) {
        if (!transport_->Read(std::chrono::steady_clock::now() + std::chrono::seconds(1))) {
            const bool was_connected = connected_.exchange(false);
            if (was_connected && disconnect_callback_) disconnect_callback_();
            break;
        }
    }
}

int EspSsl::GetLastError() { return last_error_; }
