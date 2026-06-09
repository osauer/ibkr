package main

import "testing"

func TestCheckGammaPremarketDerivedAcceptsOffHoursPricingPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  string
		want bool
	}{
		{
			name: "derived_iv_fallback",
			env:  `{"status":"ready","result":{"leg_count":12,"derived_iv_legs":7}}`,
			want: true,
		},
		{
			name: "gateway_model_tick",
			env:  `{"status":"ready","result":{"leg_count":1,"model_tick_legs":1}}`,
			want: true,
		},
		{
			name: "no_pricing_path",
			env:  `{"status":"ready","result":{"leg_count":1}}`,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkGammaPremarketDerived(checkInputs{
				Loose:         true,
				GammaEnvelope: []byte(tc.env),
			})
			if got.OK != tc.want {
				t.Fatalf("OK = %v, want %v (result=%+v)", got.OK, tc.want, got)
			}
		})
	}
}
