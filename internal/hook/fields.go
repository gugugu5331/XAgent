package hook

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type scalarKind uint8

const (
	scalarNone scalarKind = iota
	scalarString
	scalarNumber
	scalarBool
)

type fieldValue struct {
	value  any
	exists bool
	kind   scalarKind
}

type numberScalar struct {
	text string // original lossless JSON spelling; rendering expands it only within budget
}

type compiledField struct {
	raw      string
	segments []string
}

var commonFields = []string{"schema_version", "event", "sequence", "occurred_at", "project.root"}

func eventFieldCatalog(event Event) map[string]bool {
	fields := map[string]bool{}
	for _, p := range commonFields {
		fields[p] = true
	}
	add := func(paths ...string) {
		for _, p := range paths {
			fields[p] = true
		}
	}
	switch event {
	case EventSessionStart:
		add("session.id", "session.state")
	case EventSessionEnd:
		add("session.id", "session.end_reason")
	case EventTurnStart:
		addExecutionFields(add)
		add("session.id", "turn.id")
	case EventTurnEnd:
		addExecutionFields(add)
		add("session.id", "turn.id", "turn.status", "turn.error")
	case EventMessageBefore, EventMessageAfter:
		addExecutionFields(add)
		add("session.id", "turn.id", "message.id", "message.role", "message.content")
	case EventToolBefore:
		addExecutionFields(add)
		add("session.id", "turn.id", "tool.call_id", "tool.name", "tool.arguments")
	case EventToolAfter:
		addExecutionFields(add)
		add("session.id", "turn.id", "tool.call_id", "tool.name", "tool.arguments", "tool.status", "tool.duration_ms", "tool.result.content", "tool.result.error.code", "tool.result.error.message", "tool.result.error.recoverable")
	case EventCompactBefore:
		add("session.id", "execution.id", "execution.kind", "execution.mode", "turn.id", "compact.reason", "compact.before.messages", "compact.before.estimated_tokens")
	case EventCompactAfter:
		add("session.id", "execution.id", "execution.kind", "execution.mode", "turn.id", "compact.reason", "compact.before.messages", "compact.before.estimated_tokens", "compact.status", "compact.after.messages", "compact.after.estimated_tokens", "compact.error")
	}
	return fields
}

func addExecutionFields(add func(...string)) { add("execution.id", "execution.kind", "execution.mode") }

func compileField(event Event, path string) (compiledField, error) {
	if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
		return compiledField{}, fmt.Errorf("invalid field path")
	}
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if segment == "" || strings.ContainsAny(segment, "[]{} \t\r\n") {
			return compiledField{}, fmt.Errorf("invalid field path")
		}
	}
	catalog := eventFieldCatalog(event)
	if catalog[path] {
		return compiledField{raw: path, segments: segments}, nil
	}
	if (event == EventToolBefore || event == EventToolAfter) && strings.HasPrefix(path, "tool.arguments.") && len(segments) > 2 {
		return compiledField{raw: path, segments: segments}, nil
	}
	return compiledField{}, fmt.Errorf("field %q is unavailable for %s", path, event)
}

func lookupMap(data map[string]any, path compiledField) fieldValue {
	return lookupSegments(data, path.segments)
}

func lookupSegments(data map[string]any, segments []string) fieldValue {
	var current any = data
	for _, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok {
			return fieldValue{}
		}
		current, ok = object[segment]
		if !ok {
			return fieldValue{}
		}
	}
	switch value := current.(type) {
	case string:
		return fieldValue{value: value, exists: true, kind: scalarString}
	case bool:
		return fieldValue{value: value, exists: true, kind: scalarBool}
	case json.Number:
		return numberFieldValue(value.String())
	case int:
		return numberFieldValue(strconv.Itoa(value))
	case int8:
		return numberFieldValue(strconv.FormatInt(int64(value), 10))
	case int16:
		return numberFieldValue(strconv.FormatInt(int64(value), 10))
	case int32:
		return numberFieldValue(strconv.FormatInt(int64(value), 10))
	case int64:
		return numberFieldValue(strconv.FormatInt(value, 10))
	case uint:
		return numberFieldValue(strconv.FormatUint(uint64(value), 10))
	case uint8:
		return numberFieldValue(strconv.FormatUint(uint64(value), 10))
	case uint16:
		return numberFieldValue(strconv.FormatUint(uint64(value), 10))
	case uint32:
		return numberFieldValue(strconv.FormatUint(uint64(value), 10))
	case uint64:
		return numberFieldValue(strconv.FormatUint(value, 10))
	case float64:
		raw, err := json.Marshal(value)
		if err != nil {
			return fieldValue{}
		}
		return numberFieldValue(string(raw))
	case float32:
		raw, err := json.Marshal(value)
		if err != nil {
			return fieldValue{}
		}
		return numberFieldValue(string(raw))
	default:
		return fieldValue{exists: true, kind: scalarNone}
	}
}

func numberFieldValue(raw string) fieldValue {
	_, ok := scanDecimalNumber(raw)
	if !ok {
		return fieldValue{}
	}
	return fieldValue{value: numberScalar{text: raw}, exists: true, kind: scalarNumber}
}

func normalizeNumber(value string) (string, bool) {
	_, ok := scanDecimalNumber(value)
	return value, ok
}

func canonicalDecimalBounded(value string, maxBytes int) (string, bool) {
	number, ok := scanDecimalNumber(value)
	if !ok || maxBytes < 0 {
		return "", false
	}
	if number.zero {
		if maxBytes < 1 {
			return "", false
		}
		return "0", true
	}
	signBytes := 0
	if number.negative {
		signBytes = 1
	}
	if number.significantDigits > maxBytes-signBytes {
		return "", false
	}
	explicitExponent, ok := number.exponent.int64()
	if !ok {
		// An exponent outside int64 cannot be canceled into a bounded output by
		// an adjustment derived from an in-memory Go string.
		return "", false
	}
	adjustment := int64(number.trailingZeros+number.significantDigits) - int64(number.fractionDigits)
	position, ok := addInt64(explicitExponent, adjustment)
	if !ok {
		return "", false
	}
	var total int64
	switch {
	case position <= 0:
		if position == minInt64 {
			return "", false
		}
		zeros := -position
		base := int64(signBytes + 2 + number.significantDigits)
		if zeros > int64(maxBytes)-base {
			return "", false
		}
		total = base + zeros
	case position >= int64(number.significantDigits):
		if position > int64(maxBytes-signBytes) {
			return "", false
		}
		total = int64(signBytes) + position
	default:
		total = int64(signBytes + number.significantDigits + 1)
	}
	if total > int64(maxBytes) {
		return "", false
	}

	var output strings.Builder
	output.Grow(int(total))
	if number.negative {
		output.WriteByte('-')
	}
	switch {
	case position <= 0:
		output.WriteString("0.")
		writeDecimalZeros(&output, int(-position))
		number.writeSignificant(&output, number.significantDigits)
	case position >= int64(number.significantDigits):
		number.writeSignificant(&output, number.significantDigits)
		writeDecimalZeros(&output, int(position)-number.significantDigits)
	default:
		number.writeSignificant(&output, int(position))
	}
	return output.String(), true
}

type decimalNumber struct {
	value             string
	negative          bool
	mantissaStart     int
	mantissaEnd       int
	firstSignificant  int
	lastSignificant   int
	significantDigits int
	fractionDigits    int
	trailingZeros     int
	zero              bool
	exponent          decimalSignedInteger
}

type decimalSignedInteger struct {
	negative  bool
	magnitude string
}

func scanDecimalNumber(value string) (decimalNumber, bool) {
	number := decimalNumber{value: value, firstSignificant: -1, lastSignificant: -1}
	if value == "" {
		return decimalNumber{}, false
	}
	index := 0
	if value[index] == '+' || value[index] == '-' {
		number.negative = value[index] == '-'
		index++
		if index == len(value) {
			return decimalNumber{}, false
		}
	}
	number.mantissaStart = index
	digitCount := 0
	dotAt := -1
	for index < len(value) && value[index] != 'e' && value[index] != 'E' {
		character := value[index]
		switch {
		case character >= '0' && character <= '9':
			if character != '0' {
				if number.firstSignificant < 0 {
					number.firstSignificant = digitCount
				}
				number.lastSignificant = digitCount
			}
			digitCount++
		case character == '.' && dotAt < 0:
			dotAt = digitCount
		default:
			return decimalNumber{}, false
		}
		index++
	}
	number.mantissaEnd = index
	if digitCount == 0 {
		return decimalNumber{}, false
	}
	if dotAt >= 0 {
		number.fractionDigits = digitCount - dotAt
	}
	if number.firstSignificant < 0 {
		number.zero = true
		number.negative = false
	} else {
		number.significantDigits = number.lastSignificant - number.firstSignificant + 1
		number.trailingZeros = digitCount - number.lastSignificant - 1
	}

	if index == len(value) {
		return number, true
	}
	index++
	if index == len(value) {
		return decimalNumber{}, false
	}
	if value[index] == '+' || value[index] == '-' {
		number.exponent.negative = value[index] == '-'
		index++
		if index == len(value) {
			return decimalNumber{}, false
		}
	}
	exponentStart := index
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index != len(value) || index == exponentStart {
		return decimalNumber{}, false
	}
	for exponentStart < index && value[exponentStart] == '0' {
		exponentStart++
	}
	if exponentStart == index {
		number.exponent.negative = false
	} else {
		number.exponent.magnitude = value[exponentStart:index]
	}
	return number, true
}

func equalNormalizedNumber(left, right string) bool {
	a, ok := scanDecimalNumber(left)
	if !ok {
		return false
	}
	b, ok := scanDecimalNumber(right)
	if !ok {
		return false
	}
	if a.zero || b.zero {
		return a.zero && b.zero
	}
	if a.negative != b.negative || a.significantDigits != b.significantDigits || !equalSignificantDigits(a, b) {
		return false
	}
	aAdjustment := int64(a.trailingZeros) - int64(a.fractionDigits)
	bAdjustment := int64(b.trailingZeros) - int64(b.fractionDigits)
	return equalAdjustedDecimalInteger(a.exponent, aAdjustment, b.exponent, bAdjustment)
}

type significantIterator struct {
	number  decimalNumber
	index   int
	ordinal int
}

func (n decimalNumber) iterator() significantIterator {
	return significantIterator{number: n, index: n.mantissaStart, ordinal: -1}
}

func (i *significantIterator) next() (byte, bool) {
	for i.index < i.number.mantissaEnd {
		character := i.number.value[i.index]
		i.index++
		if character == '.' {
			continue
		}
		i.ordinal++
		if i.ordinal < i.number.firstSignificant {
			continue
		}
		if i.ordinal > i.number.lastSignificant {
			return 0, false
		}
		return character, true
	}
	return 0, false
}

func equalSignificantDigits(left, right decimalNumber) bool {
	a := left.iterator()
	b := right.iterator()
	for count := 0; count < left.significantDigits; count++ {
		aDigit, aOK := a.next()
		bDigit, bOK := b.next()
		if !aOK || !bOK || aDigit != bDigit {
			return false
		}
	}
	return true
}

func (n decimalNumber) writeSignificant(output *strings.Builder, decimalAt int) {
	iterator := n.iterator()
	for count := 0; count < n.significantDigits; count++ {
		if decimalAt > 0 && decimalAt < n.significantDigits && count == decimalAt {
			output.WriteByte('.')
		}
		digit, ok := iterator.next()
		if !ok {
			return
		}
		output.WriteByte(digit)
	}
}

func writeDecimalZeros(output *strings.Builder, count int) {
	const zeros = "0000000000000000000000000000000000000000000000000000000000000000"
	for count >= len(zeros) {
		output.WriteString(zeros)
		count -= len(zeros)
	}
	output.WriteString(zeros[:count])
}

type adjustedDecimalInteger struct {
	negative  bool
	magnitude string
	delta     int64
	zero      bool
}

func equalAdjustedDecimalInteger(left decimalSignedInteger, leftAdjustment int64, right decimalSignedInteger, rightAdjustment int64) bool {
	a := adjustDecimalInteger(left, leftAdjustment)
	b := adjustDecimalInteger(right, rightAdjustment)
	if a.zero || b.zero {
		return a.zero && b.zero
	}
	if a.negative != b.negative {
		return false
	}
	return equalMagnitudeWithDelta(a.magnitude, a.delta, b.magnitude, b.delta)
}

func adjustDecimalInteger(value decimalSignedInteger, adjustment int64) adjustedDecimalInteger {
	if value.magnitude == "" {
		return adjustedFromSmall(adjustment)
	}
	if !value.negative {
		if adjustment >= 0 {
			return adjustedDecimalInteger{magnitude: value.magnitude, delta: adjustment}
		}
		amount := absoluteInt64(adjustment)
		switch compareMagnitudeToUint(value.magnitude, amount) {
		case 1:
			return adjustedDecimalInteger{magnitude: value.magnitude, delta: adjustment}
		case 0:
			return adjustedDecimalInteger{zero: true}
		default:
			magnitude, _ := strconv.ParseUint(value.magnitude, 10, 64)
			return adjustedDecimalInteger{negative: true, magnitude: strconv.FormatUint(amount-magnitude, 10)}
		}
	}
	if adjustment <= 0 {
		amount := absoluteInt64(adjustment)
		return adjustedDecimalInteger{negative: true, magnitude: value.magnitude, delta: int64(amount)}
	}
	amount := uint64(adjustment)
	switch compareMagnitudeToUint(value.magnitude, amount) {
	case 1:
		return adjustedDecimalInteger{negative: true, magnitude: value.magnitude, delta: -adjustment}
	case 0:
		return adjustedDecimalInteger{zero: true}
	default:
		magnitude, _ := strconv.ParseUint(value.magnitude, 10, 64)
		return adjustedDecimalInteger{magnitude: strconv.FormatUint(amount-magnitude, 10)}
	}
}

func adjustedFromSmall(value int64) adjustedDecimalInteger {
	if value == 0 {
		return adjustedDecimalInteger{zero: true}
	}
	return adjustedDecimalInteger{negative: value < 0, magnitude: strconv.FormatUint(absoluteInt64(value), 10)}
}

func compareMagnitudeToUint(magnitude string, value uint64) int {
	other := strconv.FormatUint(value, 10)
	if len(magnitude) < len(other) {
		return -1
	}
	if len(magnitude) > len(other) {
		return 1
	}
	return strings.Compare(magnitude, other)
}

func absoluteInt64(value int64) uint64 {
	if value >= 0 {
		return uint64(value)
	}
	return uint64(-(value + 1)) + 1
}

func equalMagnitudeWithDelta(left string, leftDelta int64, right string, rightDelta int64) bool {
	leftIndex, rightIndex := len(left)-1, len(right)-1
	leftCarry, rightCarry := leftDelta, rightDelta
	for leftIndex >= 0 || rightIndex >= 0 {
		leftBase, rightBase := int64(0), int64(0)
		if leftIndex >= 0 {
			leftBase = int64(left[leftIndex] - '0')
			leftIndex--
		}
		if rightIndex >= 0 {
			rightBase = int64(right[rightIndex] - '0')
			rightIndex--
		}
		leftDigit, nextLeft := decimalDigitAndCarry(leftBase, leftCarry)
		rightDigit, nextRight := decimalDigitAndCarry(rightBase, rightCarry)
		if leftDigit != rightDigit {
			return false
		}
		leftCarry, rightCarry = nextLeft, nextRight
	}
	if leftCarry < 0 || rightCarry < 0 {
		return false
	}
	for leftCarry > 0 || rightCarry > 0 {
		if leftCarry%10 != rightCarry%10 {
			return false
		}
		leftCarry /= 10
		rightCarry /= 10
	}
	return true
}

func decimalDigitAndCarry(base, carry int64) (int64, int64) {
	next := carry / 10
	digit := base + carry%10
	if digit >= 10 {
		digit -= 10
		next++
	}
	if digit < 0 {
		digit += 10
		next--
	}
	return digit, next
}

const maxInt64 = int64(^uint64(0) >> 1)
const minInt64 = -maxInt64 - 1

func (e decimalSignedInteger) int64() (int64, bool) {
	if e.magnitude == "" {
		return 0, true
	}
	magnitude, err := strconv.ParseUint(e.magnitude, 10, 64)
	if err != nil {
		return 0, false
	}
	if !e.negative {
		if magnitude > uint64(maxInt64) {
			return 0, false
		}
		return int64(magnitude), true
	}
	if magnitude == uint64(maxInt64)+1 {
		return minInt64, true
	}
	if magnitude > uint64(maxInt64) {
		return 0, false
	}
	return -int64(magnitude), true
}

func addInt64(left, right int64) (int64, bool) {
	if right > 0 && left > maxInt64-right {
		return 0, false
	}
	if right < 0 && left < minInt64-right {
		return 0, false
	}
	return left + right, true
}
