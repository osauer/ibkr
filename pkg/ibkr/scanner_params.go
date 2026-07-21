package ibkr

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ScannerParameters is the parsed catalog of scan codes, location codes,
// and instruments supported by the gateway this connector is attached to.
//
// The catalog is a live gateway capability response and may vary by gateway
// version and market-data entitlements. RawXML retains the untrusted broker
// response for fields not represented by the typed slices.
type ScannerParameters struct {
	Instruments []ScannerInstrument `json:"instruments"`
	Locations   []ScannerLocation   `json:"locations"`
	ScanTypes   []ScannerScanType   `json:"scan_types"`
	RawXML      string              `json:"raw_xml,omitempty"`
}

// ScannerInstrument names one instrument group supported by the gateway. Type
// is the wire identifier passed as [ScannerSubscription.Instrument].
type ScannerInstrument struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ScannerLocation is one valid scanner location code. Code is the wire value
// passed as [ScannerSubscription.Exchange], and DisplayName is broker-provided
// display text.
type ScannerLocation struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
}

// ScannerScanType is one valid scanner metric. Code is the wire value passed as
// [ScannerSubscription.Type], DisplayName is broker-provided text, and
// Instruments contains the parsed instrument identifiers accepted by the scan.
type ScannerScanType struct {
	Code        string   `json:"code"`
	DisplayName string   `json:"display_name"`
	Instruments []string `json:"instruments,omitempty"`
}

// RunScannerParameters fetches and parses the gateway's scanner catalog. ctx
// must be non-nil, and a timeout of zero or less uses ten seconds. The returned
// value owns its typed slices and preserves the exact broker XML in RawXML.
// Because this broker request has no request ID, callers should serialize
// concurrent catalog requests.
func (c *Connector) RunScannerParameters(ctx context.Context, timeout time.Duration) (*ScannerParameters, error) {
	if !c.IsReady() {
		return nil, ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	var (
		mu      sync.Mutex
		rawXML  string
		gotData bool
	)
	done := make(chan struct{})
	var once sync.Once

	handlerID := c.conn.RegisterHandler(msgScannerParameters, func(fields []string) {
		// Dispatcher layout: [msgID, version, xml]. The version is field[1];
		// the XML body sits at field[2]. There is no reqID — reqScannerParameters
		// is a singleton call from the gateway's perspective.
		if len(fields) < 3 {
			return
		}
		mu.Lock()
		rawXML = fields[2]
		gotData = true
		mu.Unlock()
		once.Do(func() { close(done) })
	})
	defer c.conn.UnregisterHandler(msgScannerParameters, handlerID)

	msg := c.conn.encodeMsg(reqScannerParameters, "1")
	if err := c.conn.sendMessage(msg); err != nil {
		return nil, fmt.Errorf("send reqScannerParameters: %w", err)
	}

	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("scanner parameters timed out after %s", timeout)
	}

	mu.Lock()
	xmlBody := rawXML
	have := gotData
	mu.Unlock()
	if !have {
		return nil, fmt.Errorf("scanner parameters: empty response from gateway")
	}

	parsed, err := parseScannerParametersXML(xmlBody)
	if err != nil {
		return nil, fmt.Errorf("parse scanner parameters xml: %w", err)
	}
	parsed.RawXML = xmlBody
	return parsed, nil
}

// xmlScanParameterResponse mirrors enough of IBKR's response shape to
// pluck out the three lists agents need. Fields we don't surface are
// left untyped; encoding/xml ignores them and the rest of the document
// (filters, sicCodes, settings) stays in RawXML for the curious.
//
// The XML schema has not been published by IBKR but is stable in practice
// across server versions 100+. If a future gateway emits a different
// envelope, parseScannerParametersXML returns empty lists with no error;
// RawXML still lets the user diagnose.
type xmlScanParameterResponse struct {
	XMLName      xml.Name          `xml:"ScanParameterResponse"`
	Instruments  xmlInstrumentList `xml:"InstrumentList"`
	LocationTree xmlLocationTree   `xml:"LocationTree"`
	ScanTypeList xmlScanTypeList   `xml:"ScanTypeList"`
}

type xmlInstrumentList struct {
	Instruments []xmlInstrument `xml:"Instrument"`
}
type xmlInstrument struct {
	Name string `xml:"name"`
	Type string `xml:"type"`
}

// LocationTree is recursive: Location → LocationTree → Location → … .
// flattenLocations walks it depth-first into a single flat list.
type xmlLocationTree struct {
	Locations []xmlLocation `xml:"Location"`
}
type xmlLocation struct {
	LocationCode string          `xml:"locationCode"`
	DisplayName  string          `xml:"displayName"`
	Children     xmlLocationTree `xml:"LocationTree"`
}

type xmlScanTypeList struct {
	ScanTypes []xmlScanType `xml:"ScanType"`
}
type xmlScanType struct {
	DisplayName string `xml:"displayName"`
	ScanCode    string `xml:"scanCode"`
	Instruments string `xml:"instruments"`
}

func parseScannerParametersXML(body string) (*ScannerParameters, error) {
	var doc xmlScanParameterResponse
	dec := xml.NewDecoder(strings.NewReader(body))
	// Strict=false because IBKR's XML occasionally emits entities Go's
	// strict decoder rejects (mostly the legacy &nbsp; in displayName
	// strings). We do not need round-trip fidelity — RawXML preserves
	// the verbatim text for any consumer that needs it.
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}

	out := &ScannerParameters{}

	for _, in := range doc.Instruments.Instruments {
		out.Instruments = append(out.Instruments, ScannerInstrument{
			Name: strings.TrimSpace(in.Name),
			Type: strings.TrimSpace(in.Type),
		})
	}

	out.Locations = flattenLocations(doc.LocationTree.Locations, nil)

	for _, st := range doc.ScanTypeList.ScanTypes {
		out.ScanTypes = append(out.ScanTypes, scanTypeFromXML(st))
	}

	return out, nil
}

// FilterByInstrument returns scan types whose Instruments contain instrument,
// matched case-insensitively after trimming a non-empty query. An empty string
// returns all scan types. Returned values share nested slice data with p and
// must be treated as read-only.
func (p *ScannerParameters) FilterByInstrument(instrument string) []ScannerScanType {
	if instrument == "" {
		return p.ScanTypes
	}
	want := strings.ToUpper(strings.TrimSpace(instrument))
	out := make([]ScannerScanType, 0, len(p.ScanTypes))
	for _, st := range p.ScanTypes {
		for _, in := range st.Instruments {
			if strings.ToUpper(in) == want {
				out = append(out, st)
				break
			}
		}
	}
	return out
}

// flattenLocations walks the recursive location tree depth-first and
// returns a flat list of locations. acc is the accumulator from the
// recursive call. The non-recursive form would require an explicit
// stack; recursion is fine — the IBKR tree is shallow (4-5 levels).
func flattenLocations(in []xmlLocation, acc []ScannerLocation) []ScannerLocation {
	for _, loc := range in {
		code := strings.TrimSpace(loc.LocationCode)
		if code != "" {
			acc = append(acc, ScannerLocation{
				Code:        code,
				DisplayName: strings.TrimSpace(loc.DisplayName),
			})
		}
		if len(loc.Children.Locations) > 0 {
			acc = flattenLocations(loc.Children.Locations, acc)
		}
	}
	return acc
}

func scanTypeFromXML(st xmlScanType) ScannerScanType {
	code := strings.TrimSpace(st.ScanCode)
	out := ScannerScanType{
		Code:        code,
		DisplayName: strings.TrimSpace(st.DisplayName),
	}
	if inst := strings.TrimSpace(st.Instruments); inst != "" {
		for p := range strings.SplitSeq(inst, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out.Instruments = append(out.Instruments, p)
			}
		}
	}
	return out
}
