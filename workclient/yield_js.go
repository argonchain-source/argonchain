//go:build js && wasm

package workclient

import "time"

// yield hands control back to the JavaScript event loop. Go on WebAssembly
// runs on one thread and only returns to the browser when every goroutine is
// blocked, so a pure hashing loop would freeze the page (and, inside a Web
// Worker, starve the message handler that stops the miner). A one-millisecond
// sleep every few hashes costs a few percent and keeps everything responsive.
func yield() { time.Sleep(time.Millisecond) }

const yieldEvery = 4
