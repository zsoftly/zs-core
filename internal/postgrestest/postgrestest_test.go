package postgrestest

import "testing"

func TestTemplateRequiredReadsTheEnvironment(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		t.Setenv(EnvRequireTemplate, value)
		if got := templateRequired(); got != want {
			t.Errorf("%s=%q: templateRequired() = %v, want %v", EnvRequireTemplate, value, got, want)
		}
	}
}
