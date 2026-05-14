package ibkr

import (
	"strings"
	"testing"
)

// TestParseScannerParametersXML_SyntheticTree verifies the XML walker
// handles IBKR's three signature constructs: nested location trees,
// instrument lists, and comma-separated instrument tokens on scan types.
func TestParseScannerParametersXML_SyntheticTree(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<ScanParameterResponse>
  <InstrumentList>
    <Instrument><name>US Stocks</name><type>STK</type></Instrument>
    <Instrument><name>Options</name><type>OPT</type></Instrument>
  </InstrumentList>
  <LocationTree>
    <Location>
      <locationCode>STK</locationCode>
      <displayName>Stocks</displayName>
      <LocationTree>
        <Location>
          <locationCode>STK.US</locationCode>
          <displayName>US</displayName>
          <LocationTree>
            <Location>
              <locationCode>STK.US.MAJOR</locationCode>
              <displayName>Major Exchanges</displayName>
            </Location>
            <Location>
              <locationCode>STK.NASDAQ</locationCode>
              <displayName>NASDAQ</displayName>
            </Location>
          </LocationTree>
        </Location>
      </LocationTree>
    </Location>
  </LocationTree>
  <ScanTypeList>
    <ScanType>
      <displayName>Top % Gainers</displayName>
      <scanCode>TOP_PERC_GAIN</scanCode>
      <instruments>STK,ETF</instruments>
    </ScanType>
    <ScanType>
      <displayName>Most Active</displayName>
      <scanCode>MOST_ACTIVE</scanCode>
      <instruments>STK</instruments>
    </ScanType>
    <ScanType>
      <displayName>High Option Implied Vol</displayName>
      <scanCode>HIGH_OPT_IMP_VOLAT</scanCode>
      <instruments>STK</instruments>
    </ScanType>
  </ScanTypeList>
</ScanParameterResponse>`

	got, err := parseScannerParametersXML(body)
	if err != nil {
		t.Fatalf("parseScannerParametersXML: %v", err)
	}

	if got, want := len(got.Instruments), 2; got != want {
		t.Errorf("Instruments len = %d, want %d", got, want)
	}
	if got, want := got.Instruments[0].Type, "STK"; got != want {
		t.Errorf("Instruments[0].Type = %q, want %q", got, want)
	}

	// Locations must be flattened depth-first; the tree contains 4 nodes.
	wantCodes := []string{"STK", "STK.US", "STK.US.MAJOR", "STK.NASDAQ"}
	if got, want := len(got.Locations), len(wantCodes); got != want {
		t.Fatalf("Locations len = %d, want %d", got, want)
	}
	for i, want := range wantCodes {
		if got.Locations[i].Code != want {
			t.Errorf("Locations[%d].Code = %q, want %q", i, got.Locations[i].Code, want)
		}
	}

	if got, want := len(got.ScanTypes), 3; got != want {
		t.Fatalf("ScanTypes len = %d, want %d", got, want)
	}
	if got, want := got.ScanTypes[0].Code, "TOP_PERC_GAIN"; got != want {
		t.Errorf("ScanTypes[0].Code = %q, want %q", got, want)
	}
	// Instruments-list parsing: comma-separated, trimmed, both tokens present.
	if got, want := len(got.ScanTypes[0].Instruments), 2; got != want {
		t.Fatalf("ScanTypes[0].Instruments len = %d, want %d", got, want)
	}
	if got, want := got.ScanTypes[0].Instruments[1], "ETF"; got != want {
		t.Errorf("ScanTypes[0].Instruments[1] = %q, want %q", got, want)
	}
}

// FilterByInstrument is the agent-facing helper that narrows the catalog
// to one instrument type. Empty input is the pass-through; a missing
// instrument returns nothing.
func TestScannerParameters_FilterByInstrument(t *testing.T) {
	p := &ScannerParameters{
		ScanTypes: []ScannerScanType{
			{Code: "A", Instruments: []string{"STK", "ETF"}},
			{Code: "B", Instruments: []string{"OPT"}},
			{Code: "C", Instruments: []string{"STK"}},
		},
	}
	if got := p.FilterByInstrument(""); len(got) != 3 {
		t.Errorf("empty filter: got %d, want 3", len(got))
	}
	if got := p.FilterByInstrument("STK"); len(got) != 2 {
		t.Errorf("STK filter: got %d, want 2", len(got))
	}
	if got := p.FilterByInstrument("FUT"); len(got) != 0 {
		t.Errorf("missing filter: got %d, want 0", len(got))
	}
	// Case-insensitive match: "stk" should behave like "STK".
	if got := p.FilterByInstrument("stk"); len(got) != 2 {
		t.Errorf("lowercase filter: got %d, want 2", len(got))
	}
}

// Empty/garbage input must not panic and must surface as a parse error
// rather than a half-populated struct. encoding/xml rejects mismatched
// root elements thanks to XMLName on the decoded struct — that strictness
// is desirable, since silently returning empty lists would mask a future
// gateway-side schema change.
func TestParseScannerParametersXML_Empty(t *testing.T) {
	if _, err := parseScannerParametersXML(""); err == nil {
		t.Fatalf("expected error on empty xml, got nil")
	}
	if _, err := parseScannerParametersXML(`<Other><x>y</x></Other>`); err == nil {
		t.Fatalf("expected error on wrong root element, got nil")
	}
}

// RunScannerParameters must refuse to even attempt the call when the
// connector is not ready. This mirrors the IsReady gate in RunScannerSubscription.
func TestRunScannerParameters_NotReady(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// Deliberately no conn / lease / pool — IsReady() returns false.
	_, err := c.RunScannerParameters(t.Context(), 0)
	if err == nil {
		t.Fatalf("expected error when connector not ready, got nil")
	}
	if !strings.Contains(err.Error(), ErrIBKRUnavailable.Error()) {
		t.Errorf("expected ErrIBKRUnavailable, got %v", err)
	}
}
