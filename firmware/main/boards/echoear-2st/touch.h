#pragma once

#include <driver/i2c_master.h>

// Calls on_release on the polling task after a valid press and release.
// The bus and callback context must outlive the task.
void StartEchoEar2stTouch(i2c_master_bus_handle_t bus,
                         void (*on_release)(void*), void* context);
