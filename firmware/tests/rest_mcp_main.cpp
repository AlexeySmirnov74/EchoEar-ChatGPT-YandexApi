int main() {
    WebsocketProtocol ws;
    Socket socket;
    ws.websocket_ = &socket;
    Application app{&ws};
    assert(app.CanExecuteRestMcp(1));
    app.persistent_connecting_ = true;
    assert(!app.CanExecuteRestMcp(1));
    app.persistent_connecting_ = false;
    assert(!app.CanExecuteRestMcp(2));
    ws.traffic_suspended_ = true;
    assert(!app.CanExecuteRestMcp(1));
    app.SendRestMcpReply("{}", 1);
    assert(socket.sends == 0);
    ws.traffic_suspended_ = false;
    app.SendRestMcpReply("{}", 1);
    assert(socket.sends == 1);
    app.SendRestMcpReply("{}", 2);
    assert(socket.sends == 1);
    socket.succeeds = false;
    app.SendRestMcpReply("{}", 1);
    assert(socket.sends == 2); // uncertain transport failure is never retried
    socket.succeeds = true;
    std::atomic<bool> locked{false};
    std::thread holder([&] {
        std::lock_guard<std::recursive_mutex> guard(ws.websocket_mutex_);
        locked = true;
        std::this_thread::sleep_for(std::chrono::milliseconds(15));
    });
    while (!locked) std::this_thread::yield();
    app.SendRestMcpReply("{}", 1);
    holder.join();
    assert(socket.sends == 3); // busy retry after mutex becomes available
    locked = false;
    std::thread holder2([&] {
        std::lock_guard<std::recursive_mutex> guard(ws.websocket_mutex_);
        locked = true;
        std::this_thread::sleep_for(std::chrono::milliseconds(15));
        ws.socket_generation_ = 2;
    });
    while (!locked) std::this_thread::yield();
    app.SendRestMcpReply("{}", 1);
    holder2.join();
    assert(socket.sends == 3); // never send an old response on a new socket
    std::cout << "rest MCP tests passed\n";
}
