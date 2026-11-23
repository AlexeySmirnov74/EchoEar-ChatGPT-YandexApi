#pragma once

class Backlight;
class Display;

// Each function must be called once, with ownership held for the board lifetime.
Display* CreateEchoEar2stDisplay();
Backlight* CreateEchoEar2stBacklight();
