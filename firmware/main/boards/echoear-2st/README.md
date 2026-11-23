# OSTB EchoEar-2ST

Board profile for EchoEar-ChatGPT-YandexApi: EchoEar-2ST without BaseBoard,
ESP32-S3, 16 MiB flash and 8 MiB PSRAM. The firmware has been built and run on
one matching device. The display, touch input, voice conversations, brightness,
estimated battery level, alarms and display rest have been exercised during
development. Hardware revisions with similar names may use different wiring.

Ready firmware and the supported USB update procedure are described in
[the flashing guide](../../../../docs/FLASHING.md). Preserve and verify a
complete flash backup before writing. The release keeps the existing bootloader,
partition table, NVS and version 1 display assets; it is not a blank-chip image.
`CONFIG_FLASH_NONE_ASSETS=y` means the build does not supply replacement assets.

The source base is Denis-VR/xiaozhi-esp32 commit
`41c79363538cce4513a1b4375758dd2f4f650fdb`. The pin map was researched using the
[Muse OSTB EchoEar-2ST port](https://github.com/facebookincubator/muse-gadget-sdk/blob/8d32b7fa8d6d9b600ea65fc956e9418f5c778a28/esp32/devices/ostb-echoear-2st.md).
This profile is distinct from the upstream `echoear` board.

The audio implementation uses `BoxAudioCodec` with ES8311/ES7210 and four-slot
TDM input. Device AEC and other untested peripheral functions should not be
inferred from the board name. Battery percentage is an estimate, not a calibrated
fuel-gauge measurement. IMU, SD and reliable software power-off are not provided.
Display rest keeps power on so that touch wake and alarms remain available.

The board selects Russian and the `EmoteDisplay` renderer. The release preserves
the existing flash mode and resources during USB installation; compatibility
requirements and the scope of checks are recorded in the flashing guide.

The display initialization table in `echoear_display_init.h` retains its source
attribution and MIT license. The Muse hardware documentation retains its
Apache-2.0 attribution. This is an independently maintained integration.
