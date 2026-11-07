package patchfield

import (
	"encoding/json"
	"testing"
)

func TestNullableBoolTellsMissingNullAndValueApart(t *testing.T) {
	on := true
	cases := []struct {
		body    string
		applied bool
		want    *bool
	}{
		{body: `{}`, applied: false, want: &on},
		{body: `{"flag": null}`, applied: true, want: nil},
		{body: `{"flag": true}`, applied: true, want: &on},
		{body: `{"flag": false}`, applied: true, want: new(bool)},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			var patch struct {
				Flag NullableBool `json:"flag"`
			}
			if err := json.Unmarshal([]byte(tc.body), &patch); err != nil {
				t.Fatalf("decode: %v", err)
			}
			stored := true
			target := &stored
			if applied := patch.Flag.Apply(&target); applied != tc.applied {
				t.Fatalf("Apply = %t, want %t", applied, tc.applied)
			}
			switch {
			case tc.want == nil && target != nil:
				t.Fatalf("target = %t, want cleared", *target)
			case tc.want != nil && (target == nil || *target != *tc.want):
				t.Fatalf("target = %v, want %t", target, *tc.want)
			}
		})
	}
}

func TestNullableBoolRejectsNonBoolean(t *testing.T) {
	var patch struct {
		Flag NullableBool `json:"flag"`
	}
	if err := json.Unmarshal([]byte(`{"flag": "yes"}`), &patch); err == nil {
		t.Fatal("decoding a string succeeded")
	}
}

// Apply hands the caller its own copy, so a later change to the patch cannot
// reach into the stored setting.
func TestNullableBoolApplyCopiesTheValue(t *testing.T) {
	value := true
	patch := NullableBool{Set: true, Value: &value}
	var target *bool
	patch.Apply(&target)
	value = false
	if target == nil || !*target {
		t.Fatalf("target = %v, want an independent true", target)
	}
}
