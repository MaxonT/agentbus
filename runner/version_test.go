package runner

import "testing"

func TestRefuseControl(t *testing.T) {
	for _, tc := range []struct {
		control, mine string
		refuse        bool
	}{
		{"", "0.6.0", true},       // control sent no version: pre-0.6, older -> refuse
		{"0.5.10", "0.6.0", true}, // older release -> refuse
		{"0.6.0", "0.6.0", false}, // equal
		{"0.7.0", "0.6.0", false}, // newer control -> fine
		{"", "dev", false},        // a dev runner never refuses, even pre-0.6 control
		{"0.5.10", "dev", false},  // dev runner, older control -> no refuse
		{"", "", false},           // a runner with no release never refuses
	} {
		if got := refuseControl(tc.control, tc.mine); got != tc.refuse {
			t.Errorf("refuseControl(%q, %q) = %v, want %v", tc.control, tc.mine, got, tc.refuse)
		}
	}
}

func TestControlOlder(t *testing.T) {
	for _, tc := range []struct {
		control, mine string
		older         bool
	}{
		{"0.5.10", "0.6.0", true}, // control older: refuse
		{"0.6.0", "0.6.0", false}, // equal
		{"0.7.0", "0.6.0", false}, // control newer: fine, an old runner on new control
		{"0.5.9", "0.5.10", true}, // 9 < 10 as numbers, not strings
		{"0.5.10", "0.5.9", false},
		{"dev", "0.6.0", false},       // dev is never older
		{"0.6.0", "dev", false},       // a dev runner never refuses
		{"", "0.6.0", false},          // control did not say: do not refuse
		{"0.6.0-rc1", "0.6.0", false}, // a pre-release suffix is trimmed: equal
	} {
		if got := controlOlder(tc.control, tc.mine); got != tc.older {
			t.Errorf("controlOlder(%q, %q) = %v, want %v", tc.control, tc.mine, got, tc.older)
		}
	}
}
