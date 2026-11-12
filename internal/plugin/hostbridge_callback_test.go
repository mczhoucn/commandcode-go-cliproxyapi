package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestHostCallbackIDForwardedOnDo verifies that host_callback_id is sent when
// provided to doWithCallbackID for host.http.do requests.
func TestHostCallbackIDForwardedOnDo(t *testing.T) {
	var capturedPayload []byte
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			capturedPayload = payload
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("ok")}), nil
		}
		return hostOK(map[string]any{}), nil
	})

	_, _ = bridge.doWithCallbackID(context.Background(), pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     "https://api.example.com/test",
		Headers: http.Header{"X-Test": []string{"value"}},
		Body:    []byte("test body"),
	}, "callback-123")

	if len(capturedPayload) == 0 {
		t.Fatal("no payload captured")
	}
	var req hostHTTPReq
	if err := json.Unmarshal(capturedPayload, &req); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	if req.HostCallbackID != "callback-123" {
		t.Errorf("HostCallbackID = %q; want %q", req.HostCallbackID, "callback-123")
	}
	if req.Method != http.MethodPost {
		t.Errorf("Method = %q; want POST", req.Method)
	}
	if req.URL != "https://api.example.com/test" {
		t.Errorf("URL = %q; want https://api.example.com/test", req.URL)
	}
}

// TestHostCallbackIDForwardedOnDoStream verifies that host_callback_id is sent when
// provided to doStreamWithCallbackID for host.http.do_stream requests.
func TestHostCallbackIDForwardedOnDoStream(t *testing.T) {
	var capturedPayload []byte
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDoStream {
			capturedPayload = payload
			return hostOK(hostStreamStartResp{StatusCode: 200, StreamID: "stream-1"}), nil
		}
		return hostOK(map[string]any{}), nil
	})

	st, _, sid, err := bridge.doStreamWithCallbackID(context.Background(), pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     "https://api.example.com/stream",
		Headers: http.Header{"Authorization": []string{"Bearer test"}},
		Body:    []byte("stream body"),
	}, "stream-callback-456")

	if err != nil {
		t.Fatalf("doStreamWithCallbackID failed: %v", err)
	}
	if st != 200 {
		t.Errorf("status = %d; want 200", st)
	}
	if sid != "stream-1" {
		t.Errorf("streamID = %q; want stream-1", sid)
	}

	if len(capturedPayload) == 0 {
		t.Fatal("no payload captured")
	}
	var req hostHTTPReq
	if err := json.Unmarshal(capturedPayload, &req); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	if req.HostCallbackID != "stream-callback-456" {
		t.Errorf("HostCallbackID = %q; want %q", req.HostCallbackID, "stream-callback-456")
	}
	if req.Method != http.MethodPost {
		t.Errorf("Method = %q; want POST", req.Method)
	}
}

// TestInFlightCounterAddAfterTimedOutWait pins the reason inFlight is not a
// sync.WaitGroup: after a Wait gives up with counts still held, new units may
// be added and released, and a later Wait must observe the new zero point.
func TestInFlightCounterAddAfterTimedOutWait(t *testing.T) {
	var c inFlightCounter
	if !c.Wait(time.Millisecond) {
		t.Fatal("zero-value counter must report idle")
	}
	c.Add(1)
	if c.Wait(20 * time.Millisecond) {
		t.Fatal("wait must time out while a unit is held")
	}
	// Add after the timed-out wait: a WaitGroup may panic here.
	c.Add(1)
	c.Done()
	c.Done()
	if !c.Wait(time.Second) {
		t.Fatal("counter must be idle after all units are released")
	}
	// Reuse after reaching zero, with a waiter released by a concurrent Done.
	c.Add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.Done()
	}()
	if !c.Wait(time.Second) {
		t.Fatal("waiter must wake when the reused counter returns to zero")
	}
	// An unmatched Done must not panic or drive the count negative.
	c.Done()
	c.Add(1)
	if c.Wait(10 * time.Millisecond) {
		t.Fatal("unmatched Done must not offset a later Add")
	}
	c.Done()
}

// TestEmitTimeoutIndependentOfLogTimeout pins Finding 5: downstream emits
// wait out client backpressure far longer than the 5s log bound.
func TestEmitTimeoutIndependentOfLogTimeout(t *testing.T) {
	if emitTimeout < 60*time.Second {
		t.Fatalf("emitTimeout = %v, want >= 60s", emitTimeout)
	}
	old := logTimeout
	logTimeout = time.Millisecond
	defer func() { logTimeout = old }()
	if emitTimeout == logTimeout {
		t.Fatal("emitTimeout must not track logTimeout")
	}
}

// TestHostCallbackIDAbsentWhenEmpty verifies that host_callback_id is omitted
// when the string is empty, keeping compatibility with older hosts.
func TestHostCallbackIDAbsentWhenEmpty(t *testing.T) {
	var capturedPayload []byte
	bridge := NewHostBridge(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			capturedPayload = payload
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200}), nil
		}
		return hostOK(map[string]any{}), nil
	})

	_, _ = bridge.doWithCallbackID(context.Background(), pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    "https://api.example.com/get",
	}, "")

	if len(capturedPayload) == 0 {
		t.Fatal("no payload captured")
	}
	var raw map[string]any
	if err := json.Unmarshal(capturedPayload, &raw); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	// The omitempty tag should have excluded host_callback_id from the JSON
	if _, present := raw["host_callback_id"]; present {
		t.Error("host_callback_id present in JSON when it should be omitted (empty string)")
	}
}
