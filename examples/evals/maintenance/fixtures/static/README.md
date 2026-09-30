# Static HTTP handler

`node server.js <document-root> <port>` serves files under the root on loopback. `createServer(root)` is also exported for tests. The queue fixes HTTP method/query behavior, URL decoding, and containment while preserving existing HTML/text serving.

Run `node --test test.js` after changes.
