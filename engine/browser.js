// Emscripten JS library linked into the engine (--js-library). The engine's UDP traffic goes to Module.net, which the
// page wires to the WebRTC relay (web/src/engine.ts), instead of Emscripten's WebSocket socket emulation.
// Send, receive and socket options are replaced; socket, bind and addresses stay Emscripten's own.

addToLibrary({
  // options like SO_REUSEADDR mean nothing for a WebRTC channel, but the engine drops a socket that refuses them
  __syscall_setsockopt: (fd, level, optname, optval, optlen) => 0,

  __syscall_sendto__deps: ['$readSockaddr'],
  __syscall_sendto: (fd, buf, len, flags, addr, addrlen) => {
    const net = Module['net'];
    if (!net || !addr) return len; // nowhere to send: drop it like a lost datagram
    const to = readSockaddr(addr, addrlen);
    if (to.errno) return -to.errno;
    net.sendto({ data: HEAP8.subarray(buf, buf + len), ip: to.addr.split('.').map(Number), port: to.port });
    return len;
  },

  __syscall_recvfrom__deps: ['$writeSockaddr'],
  __syscall_recvfrom: (fd, buf, len, flags, addr, addrlen) => {
    const packet = Module['net']?.incoming.pull();
    if (!packet) return -{{{ cDefs.EAGAIN }}}; // the engine's socket is non-blocking
    const n = Math.min(len, packet.data.length);
    HEAP8.set(packet.data.subarray(0, n), buf);
    if (addr) writeSockaddr(addr, {{{ cDefs.AF_INET }}}, packet.ip.join('.'), packet.port, addrlen);
    return n;
  },
});
