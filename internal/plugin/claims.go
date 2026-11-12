package plugin

import (
	"errors"
	"time"
)

// credentialClaim tracks in-memory claims on API key digests, preventing duplicate
// auth files when saves are in flight or uncertain. Claims have a
// TTL (createdAt) so leaked claims are pruned automatically.
type credentialClaim struct {
	claimed   bool
	createdAt time.Time
}

// claimTTL is the lifetime of an in-memory credential claim. Claims
// expire after this duration to prevent leaked claims from blocking re-add forever
// (e.g. when OAuth session reached "delivered" but the file save is still uncertain,
// or when a manual add's AuthSave error left the claim in place). The TTL is longer
// than the 10-minute OAuth session lifetime to account for save latency.
const claimTTL = 15 * time.Minute

func (c credentialClaim) expired() bool {
	return c.claimed && time.Since(c.createdAt) > claimTTL
}

// authSaveDefinitelyFailed reports whether a HostBridge.AuthSave error proves
// the host did NOT persist the file (errAuthSaveRejected, see hostbridge.go
// AuthSave). A timeout or an undecodable reply stays uncertain, so the claim
// is kept and a retry cannot write a duplicate file.
func authSaveDefinitelyFailed(err error) bool {
	return errors.Is(err, errAuthSaveRejected)
}
