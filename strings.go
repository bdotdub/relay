package main

import "strings"

func stringsTrimSpace(value string) string {
	return strings.TrimSpace(value)
}

func stringsJoin(values []string, separator string) string {
	return strings.Join(values, separator)
}

func stringsIndex(value, substr string) int {
	return strings.Index(value, substr)
}

func stringsIndexAny(value, chars string) int {
	return strings.IndexAny(value, chars)
}

func joinParagraphs(values []string) string {
	return stringsJoin(values, "\n\n")
}
