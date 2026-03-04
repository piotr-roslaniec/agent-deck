package main

import "testing"

func TestIsValidHostName(t *testing.T) {
	valid := []string{"dev", "prod-east", "h1"}
	invalid := []string{"", "qa team", "qa/team", "qa.team", "qa:team", `qa\team`}

	for _, name := range valid {
		if !isValidHostName(name) {
			t.Fatalf("expected %q to be valid", name)
		}
	}

	for _, name := range invalid {
		if isValidHostName(name) {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}
