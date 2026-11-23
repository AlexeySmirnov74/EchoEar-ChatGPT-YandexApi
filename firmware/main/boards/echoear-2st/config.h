#pragma once

#include <driver/gpio.h>
#include <driver/spi_master.h>

// OSTB EchoEar-2ST, measured hardware documented by Muse. This is a separate
// profile from Espressif EchoEar; verify each peripheral on the target device.
#define AUDIO_INPUT_SAMPLE_RATE 24000
#define AUDIO_OUTPUT_SAMPLE_RATE 24000
#define AUDIO_INPUT_REFERENCE true
#define AUDIO_I2S_GPIO_MCLK GPIO_NUM_10
#define AUDIO_I2S_GPIO_BCLK GPIO_NUM_15
#define AUDIO_I2S_GPIO_WS GPIO_NUM_16
#define AUDIO_I2S_GPIO_DOUT GPIO_NUM_14
#define AUDIO_I2S_GPIO_DIN GPIO_NUM_13
#define AUDIO_CODEC_PA_PIN GPIO_NUM_18
#define AUDIO_CODEC_I2C_SCL_PIN GPIO_NUM_11
#define AUDIO_CODEC_I2C_SDA_PIN GPIO_NUM_12

#define ECHOEAR_LCD_HOST SPI2_HOST
#define ECHOEAR_LCD_SCLK GPIO_NUM_8
#define ECHOEAR_LCD_CS GPIO_NUM_3
#define ECHOEAR_LCD_DATA0 GPIO_NUM_4
#define ECHOEAR_LCD_DATA1 GPIO_NUM_5
#define ECHOEAR_LCD_DATA2 GPIO_NUM_6
#define ECHOEAR_LCD_DATA3 GPIO_NUM_7
#define ECHOEAR_LCD_RESET GPIO_NUM_9
#define ECHOEAR_LCD_BACKLIGHT GPIO_NUM_41
#define ECHOEAR_LCD_WIDTH 360
#define ECHOEAR_LCD_HEIGHT 360
#define ECHOEAR_TOUCH_ADDR 0x15

// GPIO42 is documented as a touch interrupt but this profile polls I2C.
// No software power latch, charge circuit or battery GPIO is assumed.
