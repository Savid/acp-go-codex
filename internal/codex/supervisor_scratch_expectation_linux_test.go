//go:build linux

package codex

// guardianWithoutScratchRootRefusal is how Linux refuses a guardian that names
// no scratch root. The case that reads it is Linux-only, so there is no
// non-Linux counterpart to this file. The containment marker lives in the kernel-side proof
// namespace rather than under the scratch root, so the refusal lands one step
// later than elsewhere: the private config the liveness supervisor reads is
// incomplete without a scratch root, and the guardian dies on that frame.
const guardianWithoutScratchRootRefusal = "private supervisor config is incomplete"
