package risk

import (
	"strings"
	"testing"
	"time"
)

func approvedConstitution() Constitution {
	return Constitution{
		Kind:          ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
			BlockEnforcement: EnforcementShadow,
		},
		Override: ConstitutionOverride{MaxDurationHours: new(24)},
		Recon: ConstitutionRecon{
			AmountTolerancePct:     new(0.5),
			AmountToleranceMin:     new(5.0),
			DateWindowBusinessDays: new(3),
			MaxReportAgeDays:       new(4),
		},
		Cadence: ConstitutionCadence{
			Morning: ConstitutionArtefact{Class: EnforcementAdvisory},
			EOD:     ConstitutionArtefact{Class: EnforcementAdvisory},
			Weekly:  ConstitutionArtefact{Class: EnforcementAdvisory},
		},
		Inventory: ConstitutionInventory{
			Rulebook: &ConstitutionPolicyPin{ID: "rulebook-v2", Version: "2"},
		},
	}
}

func TestConstitutionValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Constitution)
		wantErr string
	}{
		{"valid", func(c *Constitution) {}, ""},
		{"bad kind", func(c *Constitution) { c.Kind = "ibkr.other" }, "kind"},
		{"bad schema", func(c *Constitution) { c.SchemaVersion = 2 }, "schema_version"},
		{"empty id", func(c *Constitution) { c.PolicyID = " " }, "policy_id"},
		{"zero version", func(c *Constitution) { c.PolicyVersion = 0 }, "policy_version"},
		{"bad currency", func(c *Constitution) { c.Capital.BaseCurrency = "EURO" }, "base_currency"},
		{"negative floor", func(c *Constitution) { c.Capital.ProtectedFloor = new(-1.0) }, "protected_floor"},
		{"zero declared", func(c *Constitution) { c.Capital.DeclaredRiskCapital = new(0.0) }, "declared_risk_capital"},
		{"warn above block", func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(40.0) }, "below block"},
		{"warn out of range", func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(120.0) }, "(0, 100]"},
		{"hard rejected in v1", func(c *Constitution) { c.Drawdown.BlockEnforcement = "hard" }, "not promotable"},
		{"unknown enforcement", func(c *Constitution) { c.Drawdown.BlockEnforcement = "block-everything" }, "invalid"},
		{"bad cadence class", func(c *Constitution) { c.Cadence.Morning.Class = "mandatory" }, "only advisory"},
		{"pin missing version", func(c *Constitution) { c.Inventory.Rulebook = &ConstitutionPolicyPin{ID: "x"} }, "id and version"},
		{"negative recon tolerance", func(c *Constitution) { c.Recon.AmountToleranceMin = new(-1.0) }, "amount_tolerance_min"},
		{"zero recon window", func(c *Constitution) { c.Recon.DateWindowBusinessDays = new(0) }, "date_window_business_days"},
		{"zero report age", func(c *Constitution) { c.Recon.MaxReportAgeDays = new(0) }, "max_report_age_days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := approvedConstitution()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// Material keys never backfill: an empty file section validates but stays
// unapproved, and a partially approved policy names exactly the gaps.
func TestConstitutionUnapprovedKeys(t *testing.T) {
	empty := Constitution{Kind: ConstitutionKind, SchemaVersion: 1, PolicyID: "c", PolicyVersion: 1}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty-but-well-formed constitution must validate, got %v", err)
	}
	got := empty.UnapprovedKeys()
	want := []string{
		"capital.base_currency", "capital.protected_floor", "capital.declared_risk_capital",
		"capital.max_equity_age_minutes", "capital.max_unreconciled_days",
		"drawdown.warn_consumed_pct", "drawdown.block_consumed_pct", "override.max_duration_hours",
		"recon.amount_tolerance_pct", "recon.amount_tolerance_min",
		"recon.date_window_business_days", "recon.max_report_age_days",
	}
	if len(got) != len(want) {
		t.Fatalf("UnapprovedKeys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UnapprovedKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if keys := approvedConstitution().UnapprovedKeys(); len(keys) != 0 {
		t.Fatalf("fully approved constitution reports unapproved keys: %v", keys)
	}
}

// Every material and governance field must move the fingerprint: a
// threshold outside the fingerprint would be a silent policy change.
func TestConstitutionFingerprintCoversEveryField(t *testing.T) {
	base := approvedConstitution().FingerprintKey()
	mutations := map[string]func(*Constitution){
		"policy_version":                     func(c *Constitution) { c.PolicyVersion = 2 },
		"policy_id":                          func(c *Constitution) { c.PolicyID = "other" },
		"capital.base_currency":              func(c *Constitution) { c.Capital.BaseCurrency = "USD" },
		"capital.protected_floor":            func(c *Constitution) { c.Capital.ProtectedFloor = new(200001.0) },
		"capital.protected_floor unapproved": func(c *Constitution) { c.Capital.ProtectedFloor = nil },
		"capital.declared_risk_capital":      func(c *Constitution) { c.Capital.DeclaredRiskCapital = new(60000.0) },
		"capital.max_equity_age_minutes":     func(c *Constitution) { c.Capital.MaxEquityAgeMinutes = new(60) },
		"capital.max_unreconciled_days":      func(c *Constitution) { c.Capital.MaxUnreconciledDays = new(14) },
		"drawdown.warn_consumed_pct":         func(c *Constitution) { c.Drawdown.WarnConsumedPct = new(10.0) },
		"drawdown.block_consumed_pct":        func(c *Constitution) { c.Drawdown.BlockConsumedPct = new(25.0) },
		"drawdown.block_enforcement":         func(c *Constitution) { c.Drawdown.BlockEnforcement = EnforcementAdvisory },
		"override.max_duration_hours":        func(c *Constitution) { c.Override.MaxDurationHours = new(8) },
		"recon.amount_tolerance_pct":         func(c *Constitution) { c.Recon.AmountTolerancePct = new(1.0) },
		"recon.amount_tolerance_min":         func(c *Constitution) { c.Recon.AmountToleranceMin = new(10.0) },
		"recon.date_window_business_days":    func(c *Constitution) { c.Recon.DateWindowBusinessDays = new(5) },
		"recon.max_report_age_days":          func(c *Constitution) { c.Recon.MaxReportAgeDays = new(7) },
		"cadence.morning.class":              func(c *Constitution) { c.Cadence.Morning.Class = "" },
		"inventory.rulebook":                 func(c *Constitution) { c.Inventory.Rulebook.Version = "3" },
		"inventory.protection added":         func(c *Constitution) { c.Inventory.Protection = &ConstitutionPolicyPin{ID: "p", Version: "1"} },
	}
	for name, mutate := range mutations {
		c := approvedConstitution()
		mutate(&c)
		if c.FingerprintKey() == base {
			t.Errorf("mutation %q did not change the fingerprint", name)
		}
	}
	if approvedConstitution().FingerprintKey() != base {
		t.Fatal("fingerprint is not deterministic")
	}
}

func TestEvaluateCapitalTiers(t *testing.T) {
	c := approvedConstitution()
	now := time.Now()
	fresh := func(equity float64) *CapitalObservation {
		return &CapitalObservation{EquityBase: equity, AsOf: now.Add(-time.Minute)}
	}
	seeded := CapitalRuntime{AdjustedPeakBase: 260000, PeakAsOf: now.Add(-24 * time.Hour), Seeded: true, LastReconciledAt: now.Add(-time.Hour)}

	t.Run("nil policy is unapproved", func(t *testing.T) {
		v := EvaluateCapital(nil, seeded, fresh(260000), now)
		if v.Tier != CapitalTierUnapproved {
			t.Fatalf("tier = %s, want unapproved", v.Tier)
		}
	})
	t.Run("missing material key is unapproved, never ok", func(t *testing.T) {
		partial := c
		partial.Drawdown.BlockConsumedPct = nil
		v := EvaluateCapital(&partial, seeded, fresh(260000), now)
		if v.Tier != CapitalTierUnapproved {
			t.Fatalf("tier = %s, want unapproved", v.Tier)
		}
	})
	t.Run("no observation is unknown, never ok", func(t *testing.T) {
		v := EvaluateCapital(&c, seeded, nil, now)
		if v.Tier != CapitalTierUnknown {
			t.Fatalf("tier = %s, want unknown", v.Tier)
		}
	})
	t.Run("unseeded state is unknown", func(t *testing.T) {
		v := EvaluateCapital(&c, CapitalRuntime{LastReconciledAt: now}, fresh(260000), now)
		if v.Tier != CapitalTierUnknown {
			t.Fatalf("tier = %s, want unknown", v.Tier)
		}
	})
	t.Run("stale equity is unknown with disclosure", func(t *testing.T) {
		old := &CapitalObservation{EquityBase: 260000, AsOf: now.Add(-5 * time.Hour)}
		v := EvaluateCapital(&c, seeded, old, now)
		if v.Tier != CapitalTierUnknown || !v.EquityStale {
			t.Fatalf("tier = %s stale = %v, want unknown/true", v.Tier, v.EquityStale)
		}
	})
	t.Run("unreconciled past horizon is unknown", func(t *testing.T) {
		rt := seeded
		rt.LastReconciledAt = now.Add(-8 * 24 * time.Hour)
		v := EvaluateCapital(&c, rt, fresh(260000), now)
		if v.Tier != CapitalTierUnknown || !v.ReconcileStale {
			t.Fatalf("tier = %s reconcileStale = %v, want unknown/true", v.Tier, v.ReconcileStale)
		}
	})
	t.Run("ok warn block ladder in declared-risk units", func(t *testing.T) {
		// peak 260k, declared 50k: warn at −7.5k (15%), block at −15k (30%).
		for _, tc := range []struct {
			equity float64
			tier   string
		}{
			{258000, CapitalTierOK},    // −2k = 4%
			{252000, CapitalTierWarn},  // −8k = 16%
			{244000, CapitalTierBlock}, // −16k = 32%
		} {
			v := EvaluateCapital(&c, seeded, fresh(tc.equity), now)
			if v.Tier != tc.tier {
				t.Fatalf("equity %.0f: tier = %s, want %s (consumed %v)", tc.equity, v.Tier, tc.tier, v.ConsumedPct)
			}
		}
	})
	t.Run("effective risk capital is min of declared and equity minus floor", func(t *testing.T) {
		v := EvaluateCapital(&c, seeded, fresh(230000), now)
		if v.EffectiveRiskCapitalBase == nil || *v.EffectiveRiskCapitalBase != 30000 {
			t.Fatalf("effective = %v, want 30000 (equity-floor binds)", v.EffectiveRiskCapitalBase)
		}
		v = EvaluateCapital(&c, seeded, fresh(300000), now)
		if v.EffectiveRiskCapitalBase == nil || *v.EffectiveRiskCapitalBase != 50000 {
			t.Fatalf("effective = %v, want 50000 (declared binds)", v.EffectiveRiskCapitalBase)
		}
	})
	t.Run("external flows do not move drawdown", func(t *testing.T) {
		rt := seeded
		rt.CumExternalFlowsBase = 20000 // a 20k deposit was declared
		v := EvaluateCapital(&c, rt, fresh(278000), now)
		// adjusted = 278k − 20k = 258k against peak 260k: −2k = 4% → ok.
		if v.Tier != CapitalTierOK {
			t.Fatalf("tier = %s, want ok (deposit must not read as recovery or drawdown)", v.Tier)
		}
	})
	t.Run("latch dominates recovery", func(t *testing.T) {
		rt := seeded
		rt.BlockLatched = true
		v := EvaluateCapital(&c, rt, fresh(261000), now) // above peak again
		if v.Tier != CapitalTierBlock {
			t.Fatalf("tier = %s, want block (latch persists until human reset)", v.Tier)
		}
	})
	t.Run("latch dominates stale data too", func(t *testing.T) {
		rt := seeded
		rt.BlockLatched = true
		v := EvaluateCapital(&c, rt, nil, now)
		if v.Tier != CapitalTierBlock {
			t.Fatalf("tier = %s, want block", v.Tier)
		}
	})
}

// The explain view must cover every material key: a limit the operator can
// approve but the view does not render would be an invisible control.
func TestConstitutionLimitsCoverAllMaterialKeys(t *testing.T) {
	rows := ConstitutionLimits(nil)
	byKey := map[string]ConstitutionLimit{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	for _, key := range (&Constitution{}).UnapprovedKeys() {
		row, ok := byKey[key]
		if !ok {
			t.Errorf("material key %s has no explain row", key)
			continue
		}
		if row.Source != "unapproved" || row.Value != "unapproved" {
			t.Errorf("nil policy: %s renders %q/%q, want unapproved/unapproved", key, row.Value, row.Source)
		}
		if row.Meaning == "" {
			t.Errorf("%s has no meaning text", key)
		}
	}
	c := approvedConstitution()
	for _, r := range ConstitutionLimits(&c) {
		if r.Key == "drawdown.block_enforcement" {
			continue // value is the class itself
		}
		if _, material := byKey[r.Key]; material && r.Source == "unapproved" {
			// every material key was approved above; none may render unapproved
			for _, k := range c.UnapprovedKeys() {
				if k == r.Key {
					t.Fatalf("approved key %s still renders unapproved", r.Key)
				}
			}
		}
	}
}
