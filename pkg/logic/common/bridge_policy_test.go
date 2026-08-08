// ABOUTME: Tests for the bridge hint policy: allowlist validation, pinning, the
// ABOUTME: mid-session freeze, strict-mode clearing, and dial failover stickiness.
package common

import (
	"io"
	"log/slog"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func newBridgePolicyTestKD() *KeibiDrop {
	return &KeibiDrop{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestValidBridgeHintAddr(t *testing.T) {
	valid := []string{
		"fra1.bridge.keibidrop.com:26600",
		"bridge.keibisoft.com:26600",
		"keibidrop.com:26000",
		"eu1.Bridge.KeibiDrop.com:27000",
		"bridge.keibisoft.com.:26600", // trailing dot FQDN
	}
	for _, addr := range valid {
		if !validBridgeHintAddr(addr) {
			t.Errorf("rejected valid hint %q", addr)
		}
	}

	invalid := []string{
		"",
		"bridge.keibisoft.com",             // no port
		"evil.com:26600",                   // foreign host
		"keibidrop.com.evil.com:26600",     // allowlisted apex as a label
		"evilkeibisoft.com:26600",          // suffix without a dot boundary
		"bridge.keibisoft.com:25999",       // below the peer range
		"bridge.keibisoft.com:27001",       // above the peer range
		"bridge.keibisoft.com:0",           // zero port
		"bridge.keibisoft.com:x",           // non-numeric port
		"185.104.181.40:26600",             // IP literal
		"[::1]:26600",                      // IPv6 literal
		"bridge.keibisoft.com:26600:26600", // malformed
	}
	for _, addr := range invalid {
		if validBridgeHintAddr(addr) {
			t.Errorf("accepted invalid hint %q", addr)
		}
	}
}

func TestAcceptBridgeHint_AppliesValidHint(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	if kd.BridgeAddr != "fra1.bridge.keibidrop.com:26600" {
		t.Fatalf("hint not applied, BridgeAddr=%q", kd.BridgeAddr)
	}
}

func TestAcceptBridgeHint_RejectsForeignHost(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"

	kd.acceptBridgeHint("evil.example.com:26600", kd.logger)
	if kd.BridgeAddr != "bridge.keibisoft.com:26600" {
		t.Fatalf("foreign hint applied, BridgeAddr=%q", kd.BridgeAddr)
	}
}

func TestAcceptBridgeHint_StrictIgnoresHint(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.StrictMode = true

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	if kd.BridgeAddr != "" {
		t.Fatalf("strict mode accepted a hint, BridgeAddr=%q", kd.BridgeAddr)
	}
}

func TestAcceptBridgeHint_FrozenMidSession(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	// A live ReconnectManager marks an established session; keepalive
	// re-registers and reconnect relay lookups must not move the bridge.
	kd.ReconnectManager = session.NewReconnectManager(nil, kd.logger)

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	if kd.BridgeAddr != "bridge.keibisoft.com:26600" {
		t.Fatalf("mid-session hint applied, BridgeAddr=%q", kd.BridgeAddr)
	}
}

func TestPrepareBridgePolicy_StrictClearsBridge(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	kd.StrictMode = true

	kd.prepareBridgePolicy(kd.logger)
	if kd.BridgeAddr != "" {
		t.Fatalf("strict mode left BridgeAddr=%q", kd.BridgeAddr)
	}
	if kd.effectiveBridgeAddr() != "" {
		t.Fatalf("strict mode left effective bridge %q", kd.effectiveBridgeAddr())
	}
}

func TestPrepareBridgePolicy_CapturesBaseAndResetsFailover(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	kd.bridgeFellBack.Store(true)

	kd.prepareBridgePolicy(kd.logger)
	if kd.bridgeBase != "bridge.keibisoft.com:26600" {
		t.Fatalf("base not captured, got %q", kd.bridgeBase)
	}
	if kd.bridgeFellBack.Load() {
		t.Fatal("failover flag not reset at room entry")
	}

	// A later hint moves BridgeAddr; the base keeps the pre-hint value.
	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	if kd.bridgeBase != "bridge.keibisoft.com:26600" {
		t.Fatalf("base moved with the hint, got %q", kd.bridgeBase)
	}
	if kd.effectiveBridgeAddr() != "fra1.bridge.keibidrop.com:26600" {
		t.Fatalf("effective addr should follow the hint, got %q", kd.effectiveBridgeAddr())
	}

	// After a failover every lane must dial the base.
	kd.bridgeFellBack.Store(true)
	if kd.effectiveBridgeAddr() != "bridge.keibisoft.com:26600" {
		t.Fatalf("effective addr should be the base after failover, got %q", kd.effectiveBridgeAddr())
	}
}
