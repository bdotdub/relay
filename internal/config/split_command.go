package config

import "fmt"

func splitCommand(input string) ([]string, error) {
	var parts []string
	var current []rune
	var quote rune
	escaped := false

	flush := func() {
		if len(current) == 0 {
			return
		}
		parts = append(parts, string(current))
		current = nil
	}

	for _, char := range input {
		switch {
		case escaped:
			current = append(current, char)
			escaped = false
		case char == '\\':
			escaped = true
		case quote != 0:
			if char == quote {
				quote = 0
			} else {
				current = append(current, char)
			}
		case char == '\'' || char == '"':
			quote = char
		case char == ' ' || char == '\t' || char == '\n':
			flush()
		default:
			current = append(current, char)
		}
	}

	if escaped {
		current = append(current, '\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command %q", input)
	}
	flush()
	return parts, nil
}
