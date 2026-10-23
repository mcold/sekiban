package main

import "testing"

func TestValidGroupID(t *testing.T) {
	for _, id := range []string{"10001.14533", "14529.14533", "abc-1_2.3"} {
		if !validGroupID(id) {
			t.Errorf("validGroupID(%q) = false, want true", id)
		}
	}
	// The id is put into the proc path, so anything that could end it or
	// start another statement is refused.
	for _, id := range []string{"", "../backends", "1' OR '1", "1 2", "1;2",
		"1/2", "1\\2", "%2e%2e", "1(group)", "١.٢"} {
		if validGroupID(id) {
			t.Errorf("validGroupID(%q) = true, want false", id)
		}
	}
	long := ""
	for i := 0; i < 65; i++ {
		long += "1"
	}
	if validGroupID(long) {
		t.Error("a 65-character id is accepted")
	}
}
