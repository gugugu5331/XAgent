package config

import "testing"

func TestOptionalDistinguishesAbsentFalseAndZero(t *testing.T) {
	absentBool := Optional[bool]{}
	explicitFalse := Optional[bool]{Set: true, Value: false}
	absentNumber := Optional[int64]{}
	explicitZero := Optional[int64]{Set: true, Value: 0}

	if absentBool.Set || !explicitFalse.Set || explicitFalse.Value {
		t.Fatal("optional bool did not preserve explicit false")
	}
	if absentNumber.Set || !explicitZero.Set || explicitZero.Value != 0 {
		t.Fatal("optional number did not preserve explicit zero")
	}

	partial := PartialAppConfig{
		UI: PartialUIConfig{ShowResponseTimer: explicitFalse},
		MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
			"zero-timeout": {TimeoutMS: explicitZero},
			"inherited":    {},
		}},
	}
	if !partial.UI.ShowResponseTimer.Set || partial.UI.ShowResponseTimer.Value {
		t.Fatal("partial config lost explicit false")
	}
	if !partial.MCP.Servers["zero-timeout"].TimeoutMS.Set || partial.MCP.Servers["zero-timeout"].TimeoutMS.Value != 0 {
		t.Fatal("MCP server timeout lost explicit zero")
	}
	if partial.MCP.Servers["inherited"].TimeoutMS.Set {
		t.Fatal("absent MCP server timeout became present")
	}
}
