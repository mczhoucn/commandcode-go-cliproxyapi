package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// authListEnvelope builds an explicit host.auth.list result so the fake does not
// substitute its own backing store (hasExplicitAuthList keys off "files").
func authListEnvelope(entries ...pluginapi.HostAuthFileEntry) []byte {
	return hostOK(hostAuthListResponse{Files: entries})
}

// Finding 1: an aged claim is pruned, a fresh one survives.
func TestFinding1ExpiredClaimPruned(t *testing.T) {
	m, _ := oauthTestManager()
	m.lifeMu.Lock()
	m.credentialClaims = map[string]credentialClaim{
		"aged":  {claimed: true, createdAt: time.Now().Add(-2 * claimTTL)},
		"fresh": {claimed: true, createdAt: time.Now()},
	}
	m.pruneLoginsLocked()
	_, agedThere := m.credentialClaims["aged"]
	_, freshThere := m.credentialClaims["fresh"]
	m.lifeMu.Unlock()
	if agedThere {
		t.Fatal("expired claim not pruned")
	}
	if !freshThere {
		t.Fatal("fresh claim wrongly pruned")
	}
}

// Finding 1: a delivered session's claim outlives the session (an old session
// must never release a newer save's claim), but is no longer permanent: once the
// claim TTL elapses, prune releases it and the key can be re-added.
func TestFinding1DeliveredSessionPruneReleasesClaim(t *testing.T) {
	m, _ := oauthTestManager()
	start := loginStart(t, m)
	if loginRequest(t, m, "complete", start.State, "delivered-key").StatusCode != http.StatusAccepted {
		t.Fatal("stage failed")
	}
	if got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State}); got.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll did not deliver: %v", got.Status)
	}
	hash := authKeyHash("delivered-key")
	m.lifeMu.Lock()
	if !m.credentialClaims[hash].claimed {
		m.lifeMu.Unlock()
		t.Fatal("delivered claim missing before prune")
	}
	// Session expiry alone keeps the delivered claim (save outcome unknown).
	m.logins[start.State].expires = time.Now().Add(-time.Second)
	m.pruneLoginsLocked()
	_, sessionThere := m.logins[start.State]
	keptAfterExpiry := m.credentialClaims[hash].claimed
	// Once the claim TTL elapses too, prune releases it.
	c := m.credentialClaims[hash]
	c.createdAt = time.Now().Add(-2 * claimTTL)
	m.credentialClaims[hash] = c
	m.pruneLoginsLocked()
	releasedAfterTTL := !m.credentialClaims[hash].claimed
	m.lifeMu.Unlock()
	if sessionThere {
		t.Fatal("expired session not removed")
	}
	if !keptAfterExpiry {
		t.Fatal("delivered claim released on session expiry before the TTL")
	}
	if !releasedAfterTTL {
		t.Fatal("delivered claim not released after the claim TTL")
	}
	// The key is admissible again (no file was ever saved by the host here).
	if resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "delivered-key"})); resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-add after TTL release = %d, want 201", resp.StatusCode)
	}
}

// Finding 1: an uncertain save (undecodable host reply) keeps the claim so a
// retry cannot write a duplicate, while a definitive failure releases it.
func TestFinding1UncertainSaveKeepsClaim(t *testing.T) {
	m, f := oauthTestManager()
	old := f.responder
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthSave {
			return []byte("not-an-envelope"), nil
		}
		return old(method, payload)
	}
	resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "uncertain-key"}))
	if resp.StatusCode != http.StatusBadGateway || !m.credentialClaims[authKeyHash("uncertain-key")].claimed {
		t.Fatalf("uncertain save: status=%d, claim must be retained", resp.StatusCode)
	}
	if !authSaveDefinitelyFailed(errAuthSaveRejected) || authSaveDefinitelyFailed(errors.New("host auth save failed: timed out")) {
		t.Fatal("definitive/uncertain classification wrong")
	}
}

// Finding 2: one unreadable commandcode auth file (AuthGet fails) must not abort
// the whole duplicate check; a fresh, differently-named key still saves.
func TestFinding2UnreadableFileDoesNotBlockAdd(t *testing.T) {
	m, f := oauthTestManager()
	old := f.responder
	badName := "commandcode-key-unreadable.json"
	f.responder = func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return authListEnvelope(pluginapi.HostAuthFileEntry{
				ID: badName, AuthIndex: badName, Name: badName, Provider: ProviderID, Source: "file", Path: badName,
			}), nil
		case pluginabi.MethodHostAuthGet:
			return nil, errors.New("authget failed")
		}
		return old(method, payload)
	}
	resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "fresh-key"}))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unreadable file blocked a fresh add: %d %s", resp.StatusCode, resp.Body)
	}
}

// Finding 2: an unreadable file cannot be compared by key, but a digest-name
// collision must still be caught as a duplicate.
func TestFinding2UnreadableFileSameNameStillDuplicate(t *testing.T) {
	m, f := oauthTestManager()
	old := f.responder
	dupeName := authFileNameFromHash(authKeyHash("dupe-key"))
	f.responder = func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return authListEnvelope(pluginapi.HostAuthFileEntry{
				ID: dupeName, AuthIndex: dupeName, Name: dupeName, Provider: ProviderID, Source: "file", Path: dupeName,
			}), nil
		case pluginabi.MethodHostAuthGet:
			return nil, errors.New("authget failed")
		}
		return old(method, payload)
	}
	resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "dupe-key"}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("digest-name collision on unreadable file not detected: %d", resp.StatusCode)
	}
}

// Finding 3: in phase "ready", a transient checkExistingCredentialLocked failure
// (AuthList down) keeps phase "ready" and reports pending, retaining the claim;
// once the failure clears, the next poll delivers.
func TestFinding3TransientCheckErrorKeepsReady(t *testing.T) {
	m, f := oauthTestManager()
	start := loginStart(t, m)
	if loginRequest(t, m, "complete", start.State, "ready-key").StatusCode != http.StatusAccepted {
		t.Fatal("stage failed")
	}
	old := f.responder
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return nil, errors.New("transient failure")
		}
		return old(method, payload)
	}
	got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State})
	if got.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("transient check error did not report pending: %v", got.Status)
	}
	hash := authKeyHash("ready-key")
	m.lifeMu.Lock()
	phase := m.logins[start.State].phase
	claimed := m.credentialClaims[hash].claimed
	m.lifeMu.Unlock()
	if phase != "ready" {
		t.Fatalf("transient error changed phase to %q", phase)
	}
	if !claimed {
		t.Fatal("transient error released the claim")
	}
	// Recovery: the failure clears and the next poll delivers once.
	f.responder = old
	if got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State}); got.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("recovered poll did not deliver: %v", got.Status)
	}
}

// Finding 5: the OAuth session endpoint returns the session state (not a 5xx)
// when AuthList fails, treating presence as unknown.
func TestFinding5SessionReturnsStateOnAuthListFailure(t *testing.T) {
	m, f := oauthTestManager()
	start := loginStart(t, m)
	if loginRequest(t, m, "complete", start.State, "session-key").StatusCode != http.StatusAccepted {
		t.Fatal("stage failed")
	}
	if got := m.pollLogin(context.Background(), pluginapi.AuthLoginPollRequest{State: start.State}); got.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatal("poll did not deliver")
	}
	old := f.responder
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return nil, errors.New("authlist failed")
		}
		return old(method, payload)
	}
	resp := loginRequest(t, m, "session", start.State, "")
	if resp.StatusCode >= 500 {
		t.Fatalf("session returned 5xx on AuthList failure: %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("session body not JSON: %v", err)
	}
	if status, _ := body["status"].(string); status != "delivered" {
		t.Fatalf("session status = %q, want delivered (presence unknown)", status)
	}
}

// Finding 6: empty/null and date-only currentPeriodEnd on an active, allowed plan
// are accepted (empty = non-expiring; date-only parsed via quota.go's parser).
func TestFinding6TolerantPeriodEndAccepted(t *testing.T) {
	cases := map[string]any{"empty": "", "null-literal": "null", "date-only": "2099-12-31"}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			m, f := oauthTestManager()
			old := f.responder
			f.responder = func(method string, payload []byte) ([]byte, error) {
				if method == pluginabi.MethodHostHTTPDo {
					var req hostHTTPReq
					_ = json.Unmarshal(payload, &req)
					if strings.Contains(req.URL, accountSubscriptionPath) {
						return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: mustJSON(map[string]any{
							"success": true,
							"data":    map[string]any{"planId": "individual-goat", "status": "active", "currentPeriodEnd": end},
						})}), nil
					}
				}
				return old(method, payload)
			}
			resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "tolerant-" + name}))
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("period end %q rejected: %d %s", end, resp.StatusCode, resp.Body)
			}
		})
	}
}

// Finding 7: a runtime-only commandcode credential (AuthIndex set, Source not
// "file") is still compared by key, so a duplicate add is detected even though
// the stored name differs from the digest name.
func TestFinding7RuntimeOnlyDuplicateDetected(t *testing.T) {
	m, f := oauthTestManager()
	old := f.responder
	f.responder = func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return authListEnvelope(pluginapi.HostAuthFileEntry{
				ID: "rt-1", AuthIndex: "rt-1", Name: "custom-label.json", Provider: ProviderID, Source: "runtime",
			}), nil
		case pluginabi.MethodHostAuthGet:
			return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: "rt-1", Name: "custom-label.json",
				JSON: mustJSON(map[string]any{"type": ProviderID, "api_key": "runtime-key"})}), nil
		}
		return old(method, payload)
	}
	resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "runtime-key"}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("runtime-only duplicate not detected by key: %d %s", resp.StatusCode, resp.Body)
	}
}

// Finding 8: syncAccountEmail validates the email, aborts when the file changed
// since the first read, writes only a real change, and is a no-op when unchanged.
func TestFinding8EmailSyncValidationAndChangedFileAbort(t *testing.T) {
	m, _ := oauthTestManager()
	newFile := func(t *testing.T, key, email string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "cred.json")
		if err := os.WriteFile(path, mustJSON(map[string]any{"type": ProviderID, "api_key": key, "email": email}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("invalid email rejected", func(t *testing.T) {
		path := newFile(t, "k1", "old@example.test")
		entry := pluginapi.HostAuthFileEntry{Path: path}
		if err := m.syncAccountEmail(context.Background(), entry, "k1", "not-an-email"); err == nil {
			t.Fatal("invalid email accepted")
		}
	})

	t.Run("valid change written", func(t *testing.T) {
		path := newFile(t, "k2", "old@example.test")
		entry := pluginapi.HostAuthFileEntry{Path: path}
		if err := m.syncAccountEmail(context.Background(), entry, "k2", "new@example.test"); err != nil {
			t.Fatalf("valid change failed: %v", err)
		}
		raw, _ := os.ReadFile(path)
		var rec map[string]any
		_ = json.Unmarshal(raw, &rec)
		if rec["email"] != "new@example.test" || rec["api_key"] != "k2" {
			t.Fatalf("email not updated or key lost: %v", rec)
		}
	})

	t.Run("unchanged email is a no-op", func(t *testing.T) {
		path := newFile(t, "k3", "same@example.test")
		before, _ := os.ReadFile(path)
		entry := pluginapi.HostAuthFileEntry{Path: path}
		if err := m.syncAccountEmail(context.Background(), entry, "k3", "same@example.test"); err != nil {
			t.Fatalf("no-op returned error: %v", err)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("unchanged email rewrote the file")
		}
	})

	t.Run("changed credential aborts", func(t *testing.T) {
		path := newFile(t, "wrong-key", "old@example.test")
		entry := pluginapi.HostAuthFileEntry{Path: path}
		// The on-disk api_key no longer matches the credential we think we hold.
		if err := m.syncAccountEmail(context.Background(), entry, "expected-key", "new@example.test"); err == nil {
			t.Fatal("sync did not abort on changed credential")
		}
	})
}

// Finding 9: the upstream whoami/subscription verification runs OUTSIDE lifeMu,
// so no host.http.do callback happens while the lock is held.
func TestFinding9VerificationOutsideLifeMu(t *testing.T) {
	m, f := oauthTestManager()
	old := f.responder
	lockedDuringHTTP := 0
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			if m.lifeMu.TryLock() {
				m.lifeMu.Unlock()
			} else {
				lockedDuringHTTP++
			}
		}
		return old(method, payload)
	}
	if resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "outside-lock-key"})); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add failed: %d", resp.StatusCode)
	}
	if lockedDuringHTTP > 0 {
		t.Fatalf("verification HTTP ran while lifeMu held: %d calls", lockedDuringHTTP)
	}
}

// Finding 9: two concurrent adds of the same key must save exactly once; the
// loser sees a 409 from the claim/duplicate check under the lock.
func TestFinding9ConcurrentSameKeyAddsSaveOnce(t *testing.T) {
	m, f := oauthTestManager()
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, _ := m.addAccount(context.Background(), mustJSON(map[string]string{"api_key": "race-key"}))
			results <- resp.StatusCode
		}()
	}
	a, b := <-results, <-results
	saves := len(f.callsOf(pluginabi.MethodHostAuthSave))
	if saves != 1 {
		t.Fatalf("expected exactly one save, got %d (statuses %d/%d)", saves, a, b)
	}
	ok := (a == http.StatusCreated && b == http.StatusConflict) || (b == http.StatusCreated && a == http.StatusConflict)
	if !ok {
		t.Fatalf("expected one 201 and one 409, got %d and %d", a, b)
	}
}

// Finding 4: a zero-cap window folded to exceeded by the hierarchy must read as
// exhausted, never "unlimited", because state flags beat arithmetic.
func TestFinding4ZeroCapExceededReadsExhausted(t *testing.T) {
	b := quotaBucket("weekly", "Weekly window", zeroWindow(quotaWindow{Status: "ok", Cap: 0}))
	if b.RemainingFraction != 0 || strings.Contains(b.Description, "unlimited") || !strings.Contains(b.Description, "exhausted") {
		t.Fatalf("zero-cap exceeded bucket = %+v", b)
	}
}

// Finding 10: the month bucket uses the host's "monthly" spelling, and the
// exhaustion summary carries a unit so it does not render as a bare count.
func TestFinding10MonthlyWindowAndSummaryUnit(t *testing.T) {
	resp := quotaResponseFromUsage(quotaUsage{
		Month:  &quotaWindow{Status: "exceeded", Used: 70, Cap: 70},
		Weekly: quotaWindow{Status: "ok", Used: 1, Cap: 35},
	})
	found := false
	for _, b := range resp.Groups[0].Buckets {
		if b.Window == "month" {
			t.Fatal("bucket still uses \"month\"")
		}
		found = found || b.Window == "monthly"
	}
	if !found {
		t.Fatal("no monthly bucket")
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Unit == "" || !strings.Contains(resp.Summary[0].Label, "Monthly") {
		t.Fatalf("summary = %+v", resp.Summary)
	}
}
