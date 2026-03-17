package logx

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func SummarizeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("[redacted %d chars]", utf8.RuneCountInString(text))
}
