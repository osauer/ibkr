package flexstmt

import (
	"strings"
	"testing"
	"time"
)

const fixtureNormal = `<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="U0000000" fromDate="20260706" toDate="20260712" whenGenerated="20260713;063000">
   <CashTransactions>
    <CashTransaction transactionID="111" type="Deposits/Withdrawals" currency="EUR" fxRateToBase="1" amount="20000" dateTime="20260708;120000" settleDate="20260708" description="CASH RECEIPT" />
    <CashTransaction transactionID="112" type="Deposits/Withdrawals" currency="USD" fxRateToBase="0.925" amount="-10000" dateTime="20260709;120000" settleDate="20260710" description="DISBURSEMENT" />
    <CashTransaction transactionID="113" type="Dividends" currency="USD" fxRateToBase="0.925" amount="84.50" dateTime="20260710;120000" settleDate="20260710" description="MSFT DIV" />
    <CashTransaction transactionID="114" type="Some Future Line Type" currency="EUR" fxRateToBase="1" amount="12.34" dateTime="20260710;120000" settleDate="20260710" description="MYSTERY" />
   </CashTransactions>
   <Transfers>
    <Transfer transactionID="221" date="20260707" direction="IN" cashTransfer="0" positionAmountInBase="15000" fxRateToBase="1" description="ACATS STOCK IN" />
    <Transfer transactionID="222" date="20260707" direction="OUT" cashTransfer="0" positionAmountInBase="0" fxRateToBase="0" description="PENDING TRANSFER NO VALUE" />
   </Transfers>
   <EquitySummaryInBase>
    <EquitySummaryByReportDateInBase reportDate="20260708" total="261234.56" />
    <EquitySummaryByReportDateInBase reportDate="20260709" total="259100.10" />
   </EquitySummaryInBase>
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`

func TestParseNormalStatement(t *testing.T) {
	sts, err := Parse([]byte(fixtureNormal))
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 1 {
		t.Fatalf("statements = %d, want 1", len(sts))
	}
	st := sts[0]
	if st.AccountID != "U0000000" || !st.WhenGenerated.Equal(time.Date(2026, 7, 13, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("header = %+v", st)
	}
	if len(st.Cash) != 4 || len(st.Transfers) != 2 || len(st.Equity) != 2 {
		t.Fatalf("counts cash=%d transfers=%d equity=%d", len(st.Cash), len(st.Transfers), len(st.Equity))
	}
	byID := map[string]CashLine{}
	for _, c := range st.Cash {
		byID[c.ID] = c
	}
	dep := byID["cash-111"]
	if dep.Category != CategoryFlow || dep.AmountBase == nil || *dep.AmountBase != 20000 {
		t.Fatalf("deposit = %+v", dep)
	}
	wd := byID["cash-112"]
	if wd.Category != CategoryFlow || wd.AmountBase == nil || *wd.AmountBase != -9250 {
		t.Fatalf("fx withdrawal = %+v (want base -9250)", wd)
	}
	if wd.ValueDate.Format("2006-01-02") != "2026-07-10" {
		t.Fatalf("settleDate must win: %s", wd.ValueDate)
	}
	if div := byID["cash-113"]; div.Category != CategoryClassified {
		t.Fatalf("dividend = %+v, want classified non-flow", div)
	}
	// Unknown line types are uncategorized, never dropped and never flow.
	if myst := byID["cash-114"]; myst.Category != CategoryUncategorized {
		t.Fatalf("unknown type = %+v, want uncategorized", myst)
	}
	// In-kind transfer carries base value; valueless pending transfer does not.
	if st.Transfers[0].AmountBase == nil || *st.Transfers[0].AmountBase != 15000 {
		t.Fatalf("transfer in = %+v", st.Transfers[0])
	}
	if st.Transfers[1].AmountBase != nil {
		t.Fatalf("valueless transfer must have nil base, got %+v", st.Transfers[1])
	}
}

func TestParseRejectsServiceEnvelope(t *testing.T) {
	envelope := `<FlexStatementResponse timestamp="13 July, 2026 06:30 AM EDT"><Status>Warn</Status><ErrorCode>1019</ErrorCode><ErrorMessage>Statement generation in progress.</ErrorMessage></FlexStatementResponse>`
	if _, err := Parse([]byte(envelope)); err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("err = %v, want envelope rejection", err)
	}
}

func TestParseRejectsMalformedAndEmpty(t *testing.T) {
	if _, err := Parse([]byte("<FlexQueryResponse><oops")); err == nil {
		t.Fatal("malformed XML must error")
	}
	if _, err := Parse([]byte("<FlexQueryResponse></FlexQueryResponse>")); err == nil {
		t.Fatal("no FlexStatement must error")
	}
	bad := strings.Replace(fixtureNormal, `settleDate="20260708"`, `settleDate="garbage"`, 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("bad date must error, not default")
	}
}

func TestParseDateShapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"20260708", "2026-07-08T00:00:00Z"},
		{"2026-07-08", "2026-07-08T00:00:00Z"},
		{"20260708;063000", "2026-07-08T06:30:00Z"},
	} {
		got, err := parseFlexDate(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got.Format(time.RFC3339) != tc.want {
			t.Fatalf("%s = %s, want %s", tc.in, got.Format(time.RFC3339), tc.want)
		}
	}
	if _, err := parseFlexDate(""); err == nil {
		t.Fatal("empty date must error")
	}
}

// A line without a broker transaction id still gets a stable identity so
// restatements can supersede it.
func TestSyntheticLineIDIsStable(t *testing.T) {
	a := lineID("", "cash", "Deposits/Withdrawals", "20260708", 100, "X")
	b := lineID("", "cash", "Deposits/Withdrawals", "20260708", 100, "X")
	c := lineID("", "cash", "Deposits/Withdrawals", "20260708", 101, "X")
	if a != b || a == c || !strings.HasPrefix(a, "cash-synth-") {
		t.Fatalf("ids: %s %s %s", a, b, c)
	}
}
