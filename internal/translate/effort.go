package translate

// Reasoning effort levels differ between vendors (verified 2026-11-10):
// Claude takes low, medium, high, xhigh and max (Haiku 5.5 and Opus 5.5);
// OpenAI takes none, low, medium, high and xhigh, and minimal on some models
// but not gpt-5.4-nano. A level crosses as itself where both have it, else as
// the nearest the other has. A level neither knows passes on as sent, for the
// backend to accept or refuse.

// claudeEffort is an OpenAI effort level as Claude takes it: Claude can't
// reason less than low.
func claudeEffort(level string) string {
	switch level {
	case "none", "minimal":
		return "low"
	}
	return level
}

// openAIEffort is a Claude effort level as OpenAI takes it: OpenAI's highest
// is xhigh.
func openAIEffort(level string) string {
	if level == "max" {
		return "xhigh"
	}
	return level
}
