# Persistent-device work: isolated prototype, not deployable

The isolated `HandleWS` negotiates `features.persistent_idle` only when the
device requests it; unmodified devices retain the legacy branch. It implements
heartbeat, idle standby, pause, 32-bit listen IDs and generation-tagged TTS and
audio. On the first forwarded microphone frame the provider binds to that listen
ID. Any later listen start with a different ID closes the connection. This is
deliberately fail-closed and means a practical persistent conversation does not
yet work. Device MCP tools are disabled for negotiated devices pending safe
response association; legacy devices retain their volume support.

Blocker: `RealtimeCallbacks` currently omit response IDs and input generation.
An old response can be generated after a newer `listen:start`, and then a
callback could be incorrectly labelled with the new generation. A `response.done`
barrier alone is not sufficient: server VAD may not yet have emitted
`response.created` for buffered input, and tool calls can trigger follow-up
responses. The one-listen-per-provider rule prevents reassigning an uncertain
old response, at the cost of reconnecting after speech.

`persistentDeviceProtocol.replaceProvider` is a prototype only, not wired to
the handler. It clears the output queue under the generation lock, and output
enqueue, TTS writes and binary writes use that lock. This is not sufficient to
enable rotation: a callback can pass its initial generation test, then mutate
encoder/playback state after a provider replacement. Every callback (including
OnClose, OnStart, OnAudio, OnStop, transcripts, tools and timers), the provider
pointer swap, encoder replacement, queue delivery and input forwarding must be
fenced by the same generation transaction before rotation can be used. The
provider manager must also prove how stale input, cancellation, device-driven
auto rearm, asynchronous tool continuations, and final silence are handled.
Close old providers only after the new generation is atomically installed.

The fake-device WebSocket tests are offline and cannot establish real firmware
interoperability, audio quality, or correct OpenAI lifecycle. Additional tests
must cover adversarial callback/rotation schedules, missing provider events,
network failure and reconnect, long idle, actual microphone and firmware, and
restored MCP/volume. Do not deploy as a canary or firmware release.

`realtime_turn_guard.go` is an additional isolated design for explicit Realtime
responses, not wired into the provider or device handler. It models manual
responses with nonce metadata, response IDs, VAD item IDs, one in-flight clear,
cancel/response.done barriers and generation-locked output and MCP sends. It
intentionally forces a new provider after **any** forwarded audio: OpenAI's
public schema does not establish that `input_audio_buffer.cleared` fences all
asynchronous VAD events triggered by previous audio. A late `speech_started`
could otherwise be mistaken for the next listen generation. The documented
`audio_start_ms` includes prefix padding; behavior across clear and exact
association to client generations is not sufficiently specified here.

The guard currently handles only one response per listen ID and fails closed
when `response.done` precedes an outstanding tool reply, so it does not support
normal multi-response tool continuations. Before integration, opt-in VAD changes
must be installed and confirmed *after* negotiation, without changing legacy
sessions; all deliveryLoop and tool-triggered response.create calls must use
the same guard. A committed item survives clear; an interrupted or partly
played conversation requires verified item deletion/truncation or rotation.
No physical playback acknowledgement is assumed. MCP writes cannot be revoked
once sent; enforce a generation-locked enqueue and await replies outside lock.

References: https://developers.openai.com/api/docs/guides/realtime-conversations
and https://developers.openai.com/api/reference/resources/realtime/server-events

`realtime_manual_guard.go` models a second, separate strategy: disable OpenAI
turn detection after opt-in and verified session.updated, run a configurable
local streaming VAD, append PCM, wait for exactly one explicit commit ACK, and
correlate an explicit response with string metadata and response_id. Normal
completed turns may reuse the provider; ambiguous interruptions reset it. It
permits response.done before an asynchronous device tool result and requires a
separate correlated follow-up response. `finishPlayback` means the server's
queue has drained, not that physical playback is confirmed.

`persistent_stream_vad.go` implements experimental bounded pre-roll, streaming
chunks and burst commit/clear; `manual_clear_gate.go` bounds PCM retained while
waiting for clear ACK. A separate timer and serialized actor are still required.
`manual_pipeline_fake_test.go` has a *test-only* local-WebSocket actor exercising
fake device → Opus decode → VAD → fake Realtime (valid PCM/base64, string
metadata) → simulated MCP → follow-up → TTS/Opus. This is not HandleWS, not the
real openaiRealtimeSession, and not evidence of firmware/OpenAI interoperability.
Clear, pause, missing ACK, reconnect, full real-client tooling and both orders of
paced drain versus response.done have not been tested end-to-end in that actor.
The local VAD still needs tuning and physical tests for quiet speech, pauses and
start-of-phrase; noisy input, long turns and false triggers remain significant.

`realtime_manual_client.go` is a separate, unconnected manual-session dialer.
It sends an initial `session.update` with `turn_detection:null` on a fresh
WebSocket and accepts it only after an explicit matching `session.updated`.
Fake-WebSocket tests cover valid, missing and non-null VAD configuration,
timeout, server error and socket closure. It does not read audio or events
after handshake, implement the provider interface, or access the production path.
`manual_output_barrier.go` separately tests response.done and final server queue
drain arriving in both orders; neither test proves device playback.
