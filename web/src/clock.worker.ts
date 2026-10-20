// Ticks the main thread every 50 ms; workers are not throttled the way hidden-tab timers are.
setInterval(() => (self as unknown as { postMessage(m: number): void }).postMessage(0), 50);
export {};
