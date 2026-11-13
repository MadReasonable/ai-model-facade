package translate

import (
	"strings"
	"testing"
)

// A function message that answers no earlier function_call is refused,
// since its result can't be paired with a call.
func TestChat_RefusesALegacyResultWithoutItsCall(t *testing.T) {
	for name, translator := range map[string]Translator{"messages": chatToMessages{}} {
		_, _, err := translator.Request([]byte(`{"messages":[{"role":"function","name":"f","content":"x"}]}`))
		if err == nil || !strings.Contains(err.Error(), "answers no function_call") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
