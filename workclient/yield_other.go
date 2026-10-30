//go:build !(js && wasm)

package workclient

// yield is a no-op on native platforms, where the Go scheduler preempts.
func yield() {}

const yieldEvery = 1 << 30
