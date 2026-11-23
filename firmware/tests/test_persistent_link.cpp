#include "../main/persistent_link_policy.h"
#include "../components/esp-ml307/include/websocket_frame_limits.h"
#include <cassert>

int main() {
    PersistentLinkPolicy retry;
    assert(retry.Due(0));
    retry.Failed(1000000, 0);
    assert(!retry.Due(1999999));
    assert(retry.Due(2000000));
    retry.Failed(2000000, 0); // 2s stage: 1.5s lower bound
    assert(!retry.Due(3499999));
    assert(retry.Due(3500000));
    retry.Failed(4000000, 1000000); // 4s stage: upper bound
    assert(!retry.Due(7999999));
    assert(retry.Due(8000000));
    retry.Failed(8000000, 0); // next delay 8s
    retry.Failed(16000000, 0); // next delay 16s
    retry.Failed(32000000, 0); // capped stage: 22.5s lower bound
    assert(!retry.Due(54499999));
    assert(retry.Due(54500000));
    retry.Failed(32000000, 7500000); // capped stage: 30s upper bound
    assert(!retry.Due(61999999));
    assert(retry.Due(62000000));
    retry.RetryNow();
    assert(retry.Due(1));
    retry.Succeeded();
    retry.Failed(1000000, 0);
    assert(retry.Due(2000000));

    assert(PersistentEventIsCurrent(false, 12, 0)); // stock server compatibility
    assert(PersistentEventIsCurrent(true, 12, 12));
    assert(!PersistentEventIsCurrent(true, 12, 0));
    assert(!PersistentEventIsCurrent(true, 12, 11));
    assert(PersistentStandbyIsCurrent(true, 12, 12, true));
    assert(!PersistentStandbyIsCurrent(true, 12, 12, false)); // don't interrupt TTS
    assert(!PersistentStandbyIsCurrent(true, 13, 12, true)); // stale after next touch
    assert(!PersistentStandbyIsCurrent(false, 12, 12, true));
    assert(PersistentCallbackIsCurrent(true, 7, 7));
    assert(!PersistentCallbackIsCurrent(true, 8, 7)); // prior socket disconnect/error
    assert(!PersistentCallbackIsCurrent(false, 7, 7)); // upgrade/reboot invalidated it
    assert(!NetworkGenerationChanged(5, 5));
    assert(NetworkGenerationChanged(6, 5)); // quick disconnect/reconnect cannot be missed
    assert(NetworkGenerationChanged(7, 6)); // IP changed during connection attempt
    assert(NetworkAttemptIsCurrent(5, 5, 5));
    assert(!NetworkAttemptIsCurrent(5, 6, 5)); // event during handshake
    assert(!NetworkAttemptIsCurrent(6, 6, 5)); // event just before worker started
    assert(!NetworkAttemptIsCurrent(5, 6, 6)); // invalidation already observed
    // AEC keeps the processor running while speaking; a persistent session
    // must still start a new generation when TTS stops.
    uint32_t listen_id = 12;
    assert(PrepareListeningStart(true, true, listen_id));
    assert(listen_id == 13);
    assert(!PersistentEventIsCurrent(true, listen_id, 12)); // old tts:stop
    assert(PrepareListeningStart(true, false, listen_id));
    assert(listen_id == 14);
    listen_id = UINT32_MAX;
    assert(PrepareListeningStart(true, true, listen_id));
    assert(listen_id == 1); // ID zero is reserved
    // Preserve the legacy server's original processor-based behavior.
    assert(!PrepareListeningStart(false, true, listen_id));
    assert(listen_id == 1);
    assert(PrepareListeningStart(false, false, listen_id));
    assert(listen_id == 1);
    assert(WebSocketLength16(static_cast<char>(0x01), static_cast<char>(0xff)) == 511);
    assert(WebSocketLength16(static_cast<char>(0xff), static_cast<char>(0xff)) == 65535);
    assert(WebSocketMessageFits(65535, 1));
    assert(!WebSocketMessageFits(65535, 2));
    assert(!WebSocketMessageFits(65537, 0));
}
