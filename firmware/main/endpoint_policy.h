#pragma once
#include <algorithm>
#include <cctype>
#include <string>

namespace endpoint_policy {
struct Authority { std::string host; unsigned port = 443; };
inline bool ParseSecure(const std::string& url, const char* scheme, Authority& out) {
    const std::string prefix = std::string(scheme) + "://";
    if (url.compare(0, prefix.size(), prefix) != 0) return false;
    for (unsigned char c : url) if (c <= 0x20 || c >= 0x7f || c == '\\') return false;
    const auto begin = prefix.size();
    const auto end = url.find('/', begin);
    const auto authority = url.substr(begin, end == std::string::npos ? end : end - begin);
    if (authority.empty() || authority.find_first_of("@?#%[]") != std::string::npos) return false;
    const auto colon = authority.find(':');
    if (colon != std::string::npos && authority.find(':', colon + 1) != std::string::npos) return false;
    out.host = authority.substr(0, colon);
    if (out.host.empty() || out.host.front() == '.' || out.host.back() == '.') return false;
    for (unsigned char c : out.host)
        if (!std::isalnum(c) && c != '-' && c != '.') return false;
    std::transform(out.host.begin(), out.host.end(), out.host.begin(), [](unsigned char c) { return std::tolower(c); });
    out.port = 443;
    if (colon != std::string::npos) {
        auto port = authority.substr(colon + 1);
        if (port.empty() || (port.size() > 1 && port[0] == '0') || port.size() > 5) return false;
        unsigned value = 0;
        for (unsigned char c : port) { if (c < '0' || c > '9') return false; value = value * 10 + (c - '0'); }
        if (!value || value > 65535) return false;
        out.port = value;
    }
    return true;
}
inline bool AllowedWebsocket(const std::string& ota, const std::string& ws) {
    Authority expected, actual;
    return ParseSecure(ota, "https", expected) && ParseSecure(ws, "wss", actual) &&
           expected.host == actual.host && expected.port == actual.port;
}
inline bool AllowedUpgrade(const std::string& ota, const std::string& url) {
    Authority expected, actual;
    return ParseSecure(ota, "https", expected) && ParseSecure(url, "https", actual) &&
           expected.host == actual.host && expected.port == actual.port;
}
inline bool NeedOtaRefresh(bool previous_failure, const std::string& ota, const std::string& cached_ws) {
    return previous_failure || !AllowedWebsocket(ota, cached_ws);
}
inline bool AllowedExternalHttp(const std::string& url) {
    const auto sep = url.find("://");
    if (sep == std::string::npos) return false;
    std::string scheme = url.substr(0, sep);
    std::transform(scheme.begin(), scheme.end(), scheme.begin(), [](unsigned char c) { return std::tolower(c); });
    if (scheme != "http" && scheme != "https") return false;
    for (unsigned char c : url) if (c <= 0x20 || c >= 0x7f || c == '\\') return false;
    const auto begin = sep + 3, end = url.find('/', begin);
    auto authority = url.substr(begin, end == std::string::npos ? end : end - begin);
    if (authority.empty() || authority.find_first_of("@?#%[]") != std::string::npos) return false;
    auto colon = authority.find(':');
    if (colon != std::string::npos && authority.find(':', colon + 1) != std::string::npos) return false;
    auto host = authority.substr(0, colon);
    if (host.empty() || host.front() == '.' || host.back() == '.') return false;
    for (unsigned char c : host) if (!std::isalnum(c) && c != '-' && c != '.') return false;
    if (colon != std::string::npos) {
        const auto port = authority.substr(colon + 1);
        if (port.empty() || port.size() > 5 || (port.size() > 1 && port[0] == '0')) return false;
        unsigned number = 0;
        for (unsigned char c : port) { if (c < '0' || c > '9') return false; number = number * 10 + c - '0'; }
        if (!number || number > 65535) return false;
    }
    std::transform(host.begin(), host.end(), host.begin(), [](unsigned char c) { return std::tolower(c); });
    for (const std::string old_domain : {"tenclass.net", "xiaozhi.me"}) {
        if (host == old_domain || (host.size() > old_domain.size() &&
            host.compare(host.size() - old_domain.size(), old_domain.size(), old_domain) == 0 &&
            host[host.size() - old_domain.size() - 1] == '.')) return false;
    }
    return true;
}
// A non-empty user setting is authoritative: invalid values fail closed and
// must never silently fall back to a different server.
inline std::string DeploymentUrl(const std::string& configured, const std::string& fallback) {
    const auto& selected = configured.empty() ? fallback : configured;
    Authority authority;
    if (!ParseSecure(selected, "https", authority) || !AllowedExternalHttp(selected) ||
        selected.find('#') != std::string::npos) return {};
    return selected;
}
inline bool ValidProvisionedWebsocket(const std::string& ota, const std::string& ws, const std::string& token) {
    if (!AllowedWebsocket(ota, ws) || token.empty() || token.size() > 1024) return false;
    for (unsigned char c : token) if (c < 0x20 || c >= 0x7f) return false;
    return true;
}
inline bool AcceptUpgradeStatus(int status) { return status == 200; }
inline bool OldMqttEndpoint(const std::string& endpoint) {
    const auto colon = endpoint.find(':');
    const auto host = endpoint.substr(0, colon);
    std::string lower = host;
    std::transform(lower.begin(), lower.end(), lower.begin(), [](unsigned char c) { return std::tolower(c); });
    return lower == "api.tenclass.net";
}
}
