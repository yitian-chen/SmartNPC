'use strict';
// Promptfoo 0.125.1 view exposes only --port and calls listen(port).
// Scope this preload to the viewer process; pin every TCP listener to loopback.
const net = require('node:net');
const listen = net.Server.prototype.listen;
net.Server.prototype.listen = function (...args) {
  const first = args[0];
  if (first && typeof first === 'object' && Object.hasOwn(first, 'port')) {
    args[0] = { ...first, host: '127.0.0.1' };
  } else if (typeof first === 'number' || typeof first === 'string' && /^\d+$/.test(first)) {
    const callback = args.find(a => typeof a === 'function');
    args = [{ port: Number(first), host: '127.0.0.1' }, ...(callback ? [callback] : [])];
  } else {
    throw new Error('Local viewer only permits loopback TCP listeners');
  }
  return listen.apply(this, args);
};
