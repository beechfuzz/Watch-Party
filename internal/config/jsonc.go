package config

// stripJSONC removes // line comments and /* */ block comments from src,
// returning valid JSON that encoding/json can parse. It is string-literal
// aware: a "//" or "/*" inside a JSON string value (e.g.
// "public_url": "https://emby.example.com") is left untouched, since a
// naive strip would mutilate every URL in the file.
//
// Line comments are replaced with nothing, leaving the terminating newline
// in place so any later JSON parse error still reports a meaningful line
// number. Block comments are replaced with a single space rather than
// nothing, so a comment sitting directly between two tokens with no other
// whitespace doesn't glue them together (e.g. "1/*c*/2" -> "1 2", not
// "12").
//
// Trailing commas are deliberately not handled here -- only comment syntax
// was asked for, and encoding/json still rejects a trailing comma left
// behind after stripping. A future config.jsonc writer must not emit one.
func stripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false

	for i := 0; i < len(src); i++ {
		c := src[i]

		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
			// Land on the newline (or end of input) so the outer loop's
			// i++ steps past it correctly; the newline itself, if any,
			// is preserved by falling through to the next iteration.
			i--
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // now at the '/' of the closing "*/" (or past end)
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}

	return out
}
