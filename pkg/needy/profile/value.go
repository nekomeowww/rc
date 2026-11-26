package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nekomeowww/rc/pkg/utils/humantime"
)

// Values users write in profiles, kept in the form they were written so
// that re-encoding does not change a profile's text more than needed.

// isJSONNull reports whether data is the JSON literal null.
func isJSONNull(data []byte) bool {
	return string(bytes.TrimSpace(data)) == jsonNull
}

// strictUnmarshal decodes data into v, refusing unknown fields and trailing
// data, as the profile decoder does at the top level.
func strictUnmarshal(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("unexpected data after a value")
	}
	return nil
}

// jsonKind is the JSON type of a literal: '"', '{', '[', 't'/'f', 'n' or a
// number ('0').
func jsonKind(data []byte) byte {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return 0
	}
	switch c := data[0]; c {
	case '"', '{', '[', 'n':
		return c
	case 't', 'f':
		return 't'
	}
	return '0'
}

// isIntegerLiteral reports whether a JSON number literal is an integer, as
// needy's YAML reader tells integers from floats.
func isIntegerLiteral(literal string) bool {
	return !strings.ContainsAny(literal, ".eE")
}

// Duration is a span of time: `500ms`, `2s`, `10m` or `3d4h`; a bare number
// is seconds.
type Duration struct {
	text   string
	number bool
}

// NewDuration is d written as Go formats it, such as `1m30s`.
func NewDuration(d time.Duration) Duration {
	return Duration{text: d.String()}
}

// ParseDuration checks text the way needy reads a duration string.
func ParseDuration(text string) (Duration, error) {
	if _, _, err := durationFromString(text); err != nil {
		return Duration{}, err
	}
	return Duration{text: text}, nil
}

// String is the duration as written.
func (d Duration) String() string {
	return d.text
}

// Duration is the span needy reads, refused when it overflows
// time.Duration.
func (d Duration) Duration() (time.Duration, error) {
	var secs uint64
	var nanos uint32
	var err error
	if d.number {
		secs, nanos, err = durationFromNumber(d.text)
	} else {
		secs, nanos, err = durationFromString(d.text)
	}
	if err != nil {
		return 0, err
	}
	span, err := humantime.Duration{Seconds: secs, Nanoseconds: nanos}.Std()
	if err != nil {
		return 0, fmt.Errorf("duration %s overflows time.Duration: %w", d.text, err)
	}
	return span, nil
}

// MarshalJSON writes the duration as written: a string, or a number.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d.number {
		return []byte(d.text), nil
	}
	return json.Marshal(d.text)
}

// UnmarshalJSON reads a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if _, _, err := durationFromString(text); err != nil {
			return err
		}
		*d = Duration{text: text}
		return nil
	case '0':
		literal := string(bytes.TrimSpace(data))
		if _, _, err := durationFromNumber(literal); err != nil {
			return err
		}
		*d = Duration{text: literal, number: true}
		return nil
	}
	return fmt.Errorf(
		"invalid duration %s: expected a duration such as 500ms, 2s, 10m or 3d4h, or a number of seconds",
		data,
	)
}

func durationFromNumber(literal string) (uint64, uint32, error) {
	if isIntegerLiteral(literal) {
		if strings.HasPrefix(literal, "-") {
			if n, err := strconv.ParseInt(literal, 10, 64); err == nil && n == 0 {
				return 0, 0, nil
			}
			return 0, 0, fmt.Errorf("invalid duration %s", literal)
		}
		secs, err := strconv.ParseUint(literal, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid duration %s", literal)
		}
		return secs, 0, nil
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid duration %s", literal)
	}
	return durationFromSeconds(f)
}

// durationFromSeconds mirrors Rust's `Duration::try_from_secs_f64`.
func durationFromSeconds(secs float64) (uint64, uint32, error) {
	if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 || secs >= math.Ldexp(1, 64) {
		return 0, 0, fmt.Errorf("invalid duration %v", secs)
	}
	whole, frac := math.Modf(secs)
	nanos := math.Round(frac * 1e9)
	if nanos >= 1e9 {
		return uint64(whole) + 1, 0, nil
	}
	return uint64(whole), uint32(nanos), nil
}

// durationFromString reads a duration string as needy does: a number of
// seconds, or humantime's `2s`, `1m 30s`, `3d4h`.
func durationFromString(text string) (uint64, uint32, error) {
	text = strings.TrimFunc(text, unicode.IsSpace)
	if f, ok := rustParseFloatAny(text); ok {
		return durationFromSeconds(f)
	}
	d, err := humantime.Parse(text)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid duration %q: %w", text, err)
	}
	return d.Seconds, d.Nanoseconds, nil
}

// rustParseFloatAny parses text as Rust's `f64::from_str`, infinities and
// NaN included.
func rustParseFloatAny(text string) (float64, bool) {
	unsigned := strings.TrimPrefix(strings.TrimPrefix(text, "+"), "-")
	switch strings.ToLower(unsigned) {
	case "inf", "infinity":
		if strings.HasPrefix(text, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	if !rustFloat.MatchString(text) {
		return 0, false
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// ByteSize is a memory size: `512MiB`, `16GiB`, `2G`; a bare number is MiB.
type ByteSize struct {
	text   string
	number bool
}

const mib = 1 << 20

// NewByteSizeMiB is a size of mib MiB, written as a number.
func NewByteSizeMiB(n uint64) ByteSize {
	return ByteSize{text: strconv.FormatUint(n, 10), number: true}
}

// ParseByteSize checks text the way needy reads a size string.
func ParseByteSize(text string) (ByteSize, error) {
	if _, err := byteSizeFromString(text); err != nil {
		return ByteSize{}, err
	}
	return ByteSize{text: text}, nil
}

// String is the size as written.
func (s ByteSize) String() string {
	return s.text
}

// Bytes is the size needy reads.
func (s ByteSize) Bytes() (uint64, error) {
	if s.number {
		n, err := strconv.ParseUint(s.text, 10, 64)
		if err != nil {
			return 0, err
		}
		return n * mib, nil
	}
	return byteSizeFromString(s.text)
}

// MarshalJSON writes the size as written: a string, or a number of MiB.
func (s ByteSize) MarshalJSON() ([]byte, error) {
	if s.number {
		return []byte(s.text), nil
	}
	return json.Marshal(s.text)
}

// UnmarshalJSON reads a size string or a whole number of MiB.
func (s *ByteSize) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if _, err := byteSizeFromString(text); err != nil {
			return err
		}
		*s = ByteSize{text: text}
		return nil
	case '0':
		literal := string(bytes.TrimSpace(data))
		if _, err := strconv.ParseUint(literal, 10, 64); err != nil || !isIntegerLiteral(literal) {
			return fmt.Errorf("invalid size %s: expected a size such as 512MiB or 16GiB, or a number of MiB", literal)
		}
		*s = ByteSize{text: literal, number: true}
		return nil
	}
	return fmt.Errorf("invalid size %s: expected a size such as 512MiB or 16GiB, or a number of MiB", data)
}

func byteSizeFromString(text string) (uint64, error) {
	text = strings.TrimFunc(text, unicode.IsSpace)
	split := strings.IndexFunc(text, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	})
	if split < 0 {
		split = len(text)
	}
	number, ok := rustParseFloatAny(text[:split])
	if !ok {
		return 0, fmt.Errorf("invalid size %q", text)
	}
	var unit float64
	switch u := strings.TrimFunc(text[split:], unicode.IsSpace); u {
	case "", "M", "MiB", "MB":
		unit = mib
	case "G", "GiB", "GB":
		unit = 1 << 30
	case "T", "TiB", "TB":
		unit = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size unit %q in %q; use MiB, GiB or TiB", u, text)
	}
	total := math.Round(number * unit)
	if total >= math.Ldexp(1, 64) {
		return math.MaxUint64, nil
	}
	return uint64(total), nil
}

// Words is a command's words: a YAML list, or one string split on
// whitespace.
type Words struct {
	// text is the single string when written as one.
	text   string
	isText bool
	// items are the list items as JSON literals: strings, numbers or
	// booleans, which needy takes as their text.
	items []json.RawMessage
}

// NewWords is a list of words.
func NewWords(words ...string) Words {
	items := make([]json.RawMessage, 0, len(words))
	for _, word := range words {
		literal, _ := json.Marshal(word)
		items = append(items, literal)
	}
	return Words{items: items}
}

// WordsFromString is one string that needy splits on whitespace.
func WordsFromString(text string) Words {
	return Words{text: text, isText: true}
}

// IsString reports whether the words were written as one string.
func (w Words) IsString() bool {
	return w.isText
}

// Values are the words as needy reads them.
func (w Words) Values() []string {
	if w.isText {
		return strings.FieldsFunc(w.text, unicode.IsSpace)
	}
	words := make([]string, 0, len(w.items))
	for _, item := range w.items {
		word, _ := wordText(item)
		words = append(words, word)
	}
	return words
}

// MarshalJSON writes the words as written: a string, or a list.
func (w Words) MarshalJSON() ([]byte, error) {
	if w.isText {
		return json.Marshal(w.text)
	}
	if w.items == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(w.items)
}

// UnmarshalJSON reads a list of words or one string.
func (w *Words) UnmarshalJSON(data []byte) error {
	switch jsonKind(data) {
	case 'n':
		return nil
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*w = WordsFromString(text)
		return nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		for _, item := range items {
			if _, err := wordText(item); err != nil {
				return err
			}
		}
		*w = Words{items: items}
		if w.items == nil {
			w.items = []json.RawMessage{}
		}
		return nil
	}
	return fmt.Errorf("invalid words %s: expected a list of words or one space-separated string", data)
}

// wordText is a list item's text: a string, or a number or boolean as Rust
// prints it.
func wordText(item json.RawMessage) (string, error) {
	switch jsonKind(item) {
	case '"':
		var word string
		err := json.Unmarshal(item, &word)
		return word, err
	case 't':
		return string(bytes.TrimSpace(item)), nil
	case '0':
		literal := string(bytes.TrimSpace(item))
		if isIntegerLiteral(literal) {
			return literal, nil
		}
		f, err := strconv.ParseFloat(literal, 64)
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("invalid word %s: expected a word", item)
}
