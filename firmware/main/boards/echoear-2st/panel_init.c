#include <stddef.h>
#include <stdint.h>
#include <esp_lcd_st77916.h>

#include "echoear_display_init.h"

// Kept in a C translation unit because the upstream MIT-licensed panel
// sequence uses C compound literals with static storage duration.
const st77916_lcd_init_cmd_t *echoear_panel_commands(size_t *count)
{
    *count = sizeof(echoear_init_cmds) / sizeof(echoear_init_cmds[0]);
    return echoear_init_cmds;
}
