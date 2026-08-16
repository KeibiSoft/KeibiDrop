// ABOUTME: Tests for the bridge hint policy: allowlist validation, pinning, the
// ABOUTME: mid-session freeze, strict-mode clearing, and dial failover stickiness.
package common

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBridgePolicyTestKD() *KeibiDrop {
	return newBareKD()
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
		assert.True(t, validBridgeHintAddr(addr), "rejected valid hint %q", addr)
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
		assert.False(t, validBridgeHintAddr(addr), "accepted invalid hint %q", addr)
	}
}

func TestAcceptBridgeHint_AppliesValidHint(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	require.Equal(t, "fra1.bridge.keibidrop.com:26600", kd.BridgeAddr, "hint not applied")
}

func TestAcceptBridgeHint_RejectsForeignHost(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"

	kd.acceptBridgeHint("evil.example.com:26600", kd.logger)
	require.Equal(t, "bridge.keibisoft.com:26600", kd.BridgeAddr, "foreign hint applied")
}

func TestAcceptBridgeHint_StrictIgnoresHint(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.StrictMode = true

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	require.Empty(t, kd.BridgeAddr, "strict mode accepted a hint")
}

func TestAcceptBridgeHint_FrozenMidSession(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	// A live ReconnectManager marks an established session; keepalive
	// re-registers and reconnect relay lookups must not move the bridge.
	kd.ReconnectManager = session.NewReconnectManager(nil, kd.logger)

	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	require.Equal(t, "bridge.keibisoft.com:26600", kd.BridgeAddr, "mid-session hint applied")
}

func TestPrepareBridgePolicy_StrictClearsBridge(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	kd.StrictMode = true

	kd.prepareBridgePolicy(kd.logger)
	require.Empty(t, kd.BridgeAddr, "strict mode left BridgeAddr set")
	require.Empty(t, kd.effectiveBridgeAddr(), "strict mode left an effective bridge")
}

func TestPrepareBridgePolicy_CapturesBaseAndResetsFailover(t *testing.T) {
	kd := newBridgePolicyTestKD()
	kd.BridgeAddr = "bridge.keibisoft.com:26600"
	kd.bridgeFellBack.Store(true)

	kd.prepareBridgePolicy(kd.logger)
	require.Equal(t, "bridge.keibisoft.com:26600", kd.bridgeBase, "base not captured")
	require.False(t, kd.bridgeFellBack.Load(), "failover flag not reset at room entry")

	// A later hint moves BridgeAddr; the base keeps the pre-hint value.
	kd.acceptBridgeHint("fra1.bridge.keibidrop.com:26600", kd.logger)
	require.Equal(t, "bridge.keibisoft.com:26600", kd.bridgeBase, "base moved with the hint")
	require.Equal(t, "fra1.bridge.keibidrop.com:26600", kd.effectiveBridgeAddr(),
		"effective addr should follow the hint")

	// After a failover every lane must dial the base.
	kd.bridgeFellBack.Store(true)
	require.Equal(t, "bridge.keibisoft.com:26600", kd.effectiveBridgeAddr(),
		"effective addr should be the base after failover")
}
