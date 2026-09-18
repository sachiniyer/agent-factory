package redactx

// PercentDecode fully decodes a percent-encoded component while retaining a
// byte map back to the raw representation. The stable view covers NESTED
// percent encoding — %2561 decodes through %61 to 'a' — via a
// suffix-reducing stack that resolves escapes exposed by earlier decoding
// without rescanning the input, so arbitrarily nested encoding still takes
// linear work. Malformed percent bytes at the proven outer layer are ordinary
// data in the stable view.
//
// plusAsSpace belongs only to the outer application/x-www-form-urlencoded
// grammar. A plus produced from %2B is decoded data, not syntax to
// reinterpret at the next nesting depth.
//
// The returned View's Source ranges index into raw; malformedRaw reports
// that the raw representation itself contained a malformed escape, which a
// caller may treat as fail-closed rather than merely undecodable.
func PercentDecode(raw string, plusAsSpace bool) (View, bool) {
	type decodedByte struct {
		value       byte
		sourceStart int
		sourceEnd   int
	}
	stack := make([]decodedByte, 0, len(raw))
	malformedRaw := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' &&
			(i+2 >= len(raw) || !isHex(raw[i+1]) || !isHex(raw[i+2])) {
			malformedRaw = true
		}
		current := decodedByte{value: raw[i], sourceStart: i, sourceEnd: i + 1}
		if plusAsSpace && current.value == '+' {
			current.value = ' '
		}
		stack = append(stack, current)

		for len(stack) >= 3 {
			start := len(stack) - 3
			if stack[start].value != '%' {
				break
			}
			high, highOK := hexValue(stack[start+1].value)
			low, lowOK := hexValue(stack[start+2].value)
			if !highOK || !lowOK {
				break
			}
			stack = append(stack[:start], decodedByte{
				value:       high<<4 | low,
				sourceStart: stack[start].sourceStart,
				sourceEnd:   stack[start+2].sourceEnd,
			})
		}
	}
	value := make([]byte, len(stack))
	source := make([]Range, len(stack))
	for i, b := range stack {
		value[i] = b.value
		source[i] = Range{Start: b.sourceStart, End: b.sourceEnd}
	}
	return View{Text: string(value), Source: source}, malformedRaw
}

func isHex(char byte) bool {
	_, ok := hexValue(char)
	return ok
}

func hexValue(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}
