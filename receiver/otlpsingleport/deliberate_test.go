package otlpsingleport

import "testing"

func TestDeliberateFailure(t *testing.T) {
	if !deliberate(nil) {
		t.Fatal("deliberate failure: proves a failing test turns the Test report red")
	}
}
