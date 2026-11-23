# Persistent EchoEar-2ST prototype

Experimental source-only draft. Do not flash until the backend supports the
negotiated `persistent_idle` feature and the entire image is checked on the
physical EchoEar-2ST, including a tested recovery path. This port is not the
source of the original factory image.

The device negotiates `features.persistent_idle` in hello. A supported server
uses `listen_id` on `listen:start`, `listen:pause`, `standby`, `tts:start` and
`tts:stop`, and sends application `heartbeat` messages frequently enough to
refresh the 120-second data watchdog. Unsupported servers fall back to the
previous connection model. Idle capture is not sent to the server. TLS
connection attempts run in a worker with bounded exponential backoff.

Display-only dimming remains a separate prototype under
`research/echoear-display-dim`: the current Backlight driver has unsynchronized
timer/caller state and can continue a stale fade after a restore request.
Integrating that dimmer here before fixing and testing those interleavings
could leave the screen dim. This prototype makes no changes to display sleep,
power management, saved brightness, touch wiring, or the installed firmware.

The whole implementation remains subject to concurrency review, a full ESP-IDF
build, negotiated protocol integration tests, fault injection, and hardware
tests. In particular, provider rotation and server-side response/audio queue
generation isolation are implemented and tested in the server separately.
