# OSTB EchoEar-2ST — experimental board profile

This profile is a work in progress and has **not** been compiled or tested on
the target device. It is distinct from the upstream `echoear` board and from
other boards named `OSTB 2ST`. Do not flash this profile as a ready release.

The source base used for this work is Denis-VR/xiaozhi-esp32 commit
`41c79363538cce4513a1b4375758dd2f4f650fdb`. The pin map is based on the
[documented Muse OSTB EchoEar-2ST port](https://github.com/facebookincubator/muse-gadget-sdk/blob/8d32b7fa8d6d9b600ea65fc956e9418f5c778a28/esp32/devices/ostb-echoear-2st.md).
That port was tested on one unit; matching the reported SKU and peripherals
does not prove every hardware revision has identical wiring.

The stock boot log of this specific device reports QIO flash mode, which is
also the XiaoZhi ESP32-S3 default. The Muse documentation specifies DIO for
its measured unit; that setting alone does not establish that our device
requires DIO. Verify the generated flash settings before hardware testing.

The profile uses the existing XiaoZhi `BoxAudioCodec` (ES8311/ES7210 and
four-slot TDM input). The original boot log reports a similar audio setup, but
the input slot routing and reference channel must be measured before enabling
or relying on device AEC. Display rendering, touch response, audio, Wi-Fi setup,
reconnection, the default assets/fonts, and boot behavior require verification.
Battery reporting, charging, power-off control, IMU and SD are not implemented.

The board build configuration selects Russian and the `EmoteDisplay` renderer.
The original backup's asset archive has a valid checksum and a version 1
index, which the current loader accepts; it lists the EAF face animations,
icons, layout, font and speech model. The runtime compatibility of these
assets and the appearance on the actual screen are not yet verified.
`CONFIG_FLASH_NONE_ASSETS=y` keeps the build from packaging new assets, but
does not provide assets on an empty flash. Verify the actual generated flash
arguments and preserve the existing assets partition during hardware tests.

The checked-in project defaults describe the same partition offsets and sizes
as the backup of this unit, but that alone does not establish compatibility of
bootloader, NVS, OTA state or assets. Preserve and verify a current full backup
before any hardware experiment. Do not erase or overwrite the existing NVS,
OTA metadata, bootloader, partition table or assets without separately
establishing the intended recovery and compatibility procedure.

The display initialization table is attributed in `echoear_display_init.h`
and retains its original MIT license. The hardware map and control behavior
were researched using the Apache-2.0-licensed Muse port; the profile is a
new integration with XiaoZhi rather than an official or validated release.
