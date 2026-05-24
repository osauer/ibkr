package main

import "testing"

func TestCheckQuoteSPYLiveRequiresCurrentTick(t *testing.T) {
	frames := []WireFrame{
		spyReqMktDataFrame(),
		tickPriceFrame("37", "743.73"),
	}

	got := checkQuoteSPY(checkInputs{Frames: frames})
	if got.OK {
		t.Fatal("live quote-spy check accepted mark-price fallback; want current bid/ask/last tick")
	}

	frames = append(frames, tickPriceFrame("4", "743.74"))
	got = checkQuoteSPY(checkInputs{Frames: frames})
	if !got.OK {
		t.Fatalf("live quote-spy check rejected last tick: %+v", got)
	}
}

func TestCheckQuoteSPYLooseAcceptsFrozenFallbackTicks(t *testing.T) {
	for _, tickType := range []string{"9", "37"} {
		frames := []WireFrame{
			spyReqMktDataFrame(),
			tickPriceFrame(tickType, "743.73"),
		}

		got := checkQuoteSPY(checkInputs{Frames: frames, Loose: true})
		if !got.OK {
			t.Fatalf("loose quote-spy check rejected tickType %s: %+v", tickType, got)
		}
	}
}

func spyReqMktDataFrame() WireFrame {
	return WireFrame{
		Direction: "OUT",
		MsgID:     1,
		MsgName:   "reqMktData",
		Fields:    []string{"1", "11", "1", "756733", "SPY", "STK"},
	}
}

func tickPriceFrame(tickType, price string) WireFrame {
	return WireFrame{
		Direction: "IN",
		MsgID:     1,
		MsgName:   "tickPrice",
		Fields:    []string{"1", "6", "1", tickType, price, "0", "0"},
	}
}
