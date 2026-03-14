package main

func summarizeText(text string) string {
	text = stringsTrimSpace(text)
	if text == "" {
		return ""
	}
	const limit = 120
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "...<truncated>"
}
