int main() {
 Backlight b;
 b.SetBrightness(80, true);
 for (int i=0;i<5;i++) b.OnTransitionTimer();
 assert(b.brightness_==55 && b.selected_brightness_==80 && saved_brightness==80);
 assert(b.SetBlanked(true));
 for (int i=0;i<55;i++) b.OnTransitionTimer();
 assert(b.brightness_==0 && b.blanked_ && saved_brightness==80);
 b.SetBrightness(32, true);
 assert(b.target_brightness_==0 && b.selected_brightness_==32 && saved_brightness==32);
 assert(b.SetBlanked(false));
 for (int i=0;i<32;i++) b.OnTransitionTimer();
 assert(b.brightness_==32 && saved_brightness==32);
 fail_next_start=true;
 assert(!b.SetBlanked(true));
 assert(!b.blanked_ && b.target_brightness_==32 && saved_brightness==32);
 std::atomic_bool running{true};
 std::thread ticker([&] { while (running) { if (timer_active) b.OnTransitionTimer(); std::this_thread::yield(); } });
 std::thread w1([&] { for (int i=0;i<2000;i++) b.SetBrightness(10+i%80, true); });
 std::thread w2([&] { for (int i=0;i<2000;i++) { assert(b.SetBlanked(true)); assert(b.SetBlanked(false)); } });
 w1.join(); w2.join();
 assert(b.SetBlanked(false)); b.SetBrightness(71, true);
 auto deadline=std::chrono::steady_clock::now()+std::chrono::seconds(5);
 while ((b.brightness()!=71 || timer_active) && std::chrono::steady_clock::now()<deadline) std::this_thread::yield();
 running=false; ticker.join();
 assert(b.brightness()==71 && !timer_active && saved_brightness==71);
 std::puts("rest backlight target, failure and concurrent changes passed");
}
