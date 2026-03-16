package logx

import "strings"

func SummarizeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	const limit = 120
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "...<truncated>"
}
