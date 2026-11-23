#include "display.h"

#include "backlight.h"
#include "config.h"
#include "sdkconfig.h"
#if CONFIG_USE_EMOTE_MESSAGE_STYLE
#include "display/emote_display.h"
#else
#include "display/lcd_display.h"
#endif

#include <driver/spi_master.h>
#include <esp_check.h>
#include <esp_lcd_panel_io.h>
#include <esp_lcd_panel_ops.h>
#include <esp_lcd_st77916.h>

// Pin map from the measured OSTB EchoEar-2ST port in muse-gadget-sdk:
// https://github.com/facebookincubator/muse-gadget-sdk/blob/main/esp32/devices/ostb-echoear-2st.md
// The original ESP-VoCat/EchoEar board uses a different pin map.
namespace {
constexpr int kResolution = ECHOEAR_LCD_WIDTH;
constexpr spi_host_device_t kHost = ECHOEAR_LCD_HOST;
constexpr gpio_num_t kBacklightPin = ECHOEAR_LCD_BACKLIGHT;
}

extern "C" const st77916_lcd_init_cmd_t *echoear_panel_commands(size_t *count);

Display* CreateEchoEar2stDisplay()
{
    spi_bus_config_t bus = {};
    bus.sclk_io_num = ECHOEAR_LCD_SCLK;
    bus.mosi_io_num = ECHOEAR_LCD_DATA0;
    bus.miso_io_num = ECHOEAR_LCD_DATA1;
    bus.quadwp_io_num = ECHOEAR_LCD_DATA2;
    bus.quadhd_io_num = ECHOEAR_LCD_DATA3;
    bus.max_transfer_sz = kResolution * 20 * sizeof(uint16_t);
    ESP_ERROR_CHECK(spi_bus_initialize(kHost, &bus, SPI_DMA_CH_AUTO));

    esp_lcd_panel_io_spi_config_t io = {};
    io.cs_gpio_num = ECHOEAR_LCD_CS;
    io.dc_gpio_num = -1;
    io.spi_mode = 0;
    io.pclk_hz = 40 * 1000 * 1000;
    io.trans_queue_depth = 10;
    io.lcd_cmd_bits = 32;
    io.lcd_param_bits = 8;
    io.flags.quad_mode = 1;
    esp_lcd_panel_io_handle_t panel_io = nullptr;
    ESP_ERROR_CHECK(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)kHost, &io, &panel_io));

    st77916_vendor_config_t vendor = {};
    size_t command_count = 0;
    vendor.init_cmds = echoear_panel_commands(&command_count);
    ESP_ERROR_CHECK(command_count <= UINT16_MAX ? ESP_OK : ESP_ERR_INVALID_SIZE);
    vendor.init_cmds_size = static_cast<uint16_t>(command_count);
    vendor.flags.use_qspi_interface = 1;
    esp_lcd_panel_dev_config_t panel_config = {};
    panel_config.reset_gpio_num = ECHOEAR_LCD_RESET;
    panel_config.rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB;
    panel_config.bits_per_pixel = 16;
    panel_config.vendor_config = &vendor;
    esp_lcd_panel_handle_t panel = nullptr;
    ESP_ERROR_CHECK(esp_lcd_new_panel_st77916(panel_io, &panel_config, &panel));
    ESP_ERROR_CHECK(esp_lcd_panel_reset(panel));
    ESP_ERROR_CHECK(esp_lcd_panel_init(panel));
    ESP_ERROR_CHECK(esp_lcd_panel_invert_color(panel, true));
    ESP_ERROR_CHECK(esp_lcd_panel_swap_xy(panel, false));
    ESP_ERROR_CHECK(esp_lcd_panel_mirror(panel, false, false));

#if CONFIG_USE_EMOTE_MESSAGE_STYLE
    // SpiLcdDisplay normally turns the panel on in its constructor. The
    // emote renderer does not, so enable it before starting that engine.
    ESP_ERROR_CHECK(esp_lcd_panel_disp_on_off(panel, true));
    return new emote::EmoteDisplay(panel, panel_io, kResolution, kResolution);
#else
    return new SpiLcdDisplay(panel_io, panel, kResolution, kResolution, 0, 0,
                             false, false, false);
#endif
}

Backlight* CreateEchoEar2stBacklight()
{
    auto* backlight = new PwmBacklight(kBacklightPin, false, 20000);
    backlight->RestoreBrightness();
    return backlight;
}
