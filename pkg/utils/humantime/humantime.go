// Package humantime parses durations such as `2s`, `1m 30s`, or `3d4h` the
// way the Rust crate humantime does, for reading configuration written for
// Rust programs. It is a port of humantime 2.4's `parse_duration`.
// Source: https://github.com/chronotope/humantime/blob/v2.4.0/src/duration.rs
package humantime

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"
	"unicode"
)

// ErrOverflow means a duration does not fit, or a fraction is finer than a
// nanosecond.
var ErrOverflow = errors.New("number is too large or cannot be represented without a lack of precision")

// Duration is a span as Rust's `std::time::Duration` holds it. It reaches
// far beyond time.Duration: humantime accepts up to 2^64 seconds.
type Duration struct {
	Seconds     uint64
	Nanoseconds uint32
}

// Parse reads s as humantime's `parse_duration` does: a sequence of numbers
// with units, such as `1h 30m` or `1.5h`. A bare `0` is zero; any other
// number needs a unit.
func Parse(s string) (Duration, error) {
	if s == "0" {
		return Duration{}, nil
	}
	p := &parser{src: []rune(s)}
	return p.parse()
}

// Std is d as a time.Duration, or ErrOverflow when it does not fit.
func (d Duration) Std() (time.Duration, error) {
	const maxSeconds = uint64(math.MaxInt64 / int64(time.Second))
	if d.Seconds > maxSeconds ||
		d.Seconds == maxSeconds && int64(d.Nanoseconds) > math.MaxInt64%int64(time.Second) {
		return 0, ErrOverflow
	}
	return time.Duration(d.Seconds)*time.Second + time.Duration(d.Nanoseconds), nil
}

type parser struct {
	src []rune
	pos int
}

type fraction struct {
	numerator   uint64
	denominator uint64
}

func (p *parser) next() (rune, bool) {
	if p.pos >= len(p.src) {
		return 0, false
	}
	c := p.src[p.pos]
	p.pos++
	return c, true
}

func isUnitRune(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == 'µ'
}

func (p *parser) parse() (Duration, error) {
	n, ok, err := p.firstChar()
	if err != nil {
		return Duration{}, err
	}
	if !ok {
		return Duration{}, errors.New("value was empty")
	}
	var out Duration
	for {
		var frac *fraction
		off := p.pos
	number:
		for {
			c, more := p.next()
			if !more {
				break
			}
			switch {
			case c >= '0' && c <= '9':
				n, err = checkedMulAdd(n, 10, uint64(c-'0'))
				if err != nil {
					return Duration{}, err
				}
			case unicode.IsSpace(c):
			case isUnitRune(c):
				break number
			case c == '.':
				f, err := p.parseFraction(&off)
				if err != nil {
					return Duration{}, err
				}
				frac = &f
				break number
			default:
				return Duration{}, fmt.Errorf("invalid character at %d", off)
			}
			off = p.pos
		}
		start := off
		off = p.pos
		continued := false
	unit:
		for {
			c, more := p.next()
			if !more {
				break
			}
			switch {
			case c >= '0' && c <= '9':
				if err := out.addUnit(p.src[start:off], n, frac); err != nil {
					return Duration{}, err
				}
				n = uint64(c - '0')
				continued = true
				break unit
			case unicode.IsSpace(c):
				break unit
			case isUnitRune(c):
			default:
				return Duration{}, fmt.Errorf("invalid character at %d", off)
			}
			off = p.pos
		}
		if continued {
			continue
		}
		if err := out.addUnit(p.src[start:off], n, frac); err != nil {
			return Duration{}, err
		}
		n, ok, err = p.firstChar()
		if err != nil {
			return Duration{}, err
		}
		if !ok {
			return out, nil
		}
	}
}

func (p *parser) firstChar() (uint64, bool, error) {
	off := p.pos
	for {
		c, more := p.next()
		if !more {
			return 0, false, nil
		}
		switch {
		case c >= '0' && c <= '9':
			return uint64(c - '0'), true, nil
		case unicode.IsSpace(c):
			continue
		default:
			return 0, false, fmt.Errorf("expected number at %d", off)
		}
	}
}

func (p *parser) parseFraction(off *int) (fraction, error) {
	var numerator uint64
	denominator := uint64(1)
	zeros := true
	var err error
loop:
	for {
		c, more := p.next()
		if !more {
			break
		}
		switch {
		case c == '0':
			if denominator, err = checkedMulAdd(denominator, 10, 0); err != nil {
				return fraction{}, err
			}
			if !zeros {
				if numerator, err = checkedMulAdd(numerator, 10, 0); err != nil {
					return fraction{}, err
				}
			}
		case c >= '1' && c <= '9':
			zeros = false
			if denominator, err = checkedMulAdd(denominator, 10, 0); err != nil {
				return fraction{}, err
			}
			if numerator, err = checkedMulAdd(numerator, 10, uint64(c-'0')); err != nil {
				return fraction{}, err
			}
		case unicode.IsSpace(c):
		case isUnitRune(c):
			break loop
		default:
			return fraction{}, fmt.Errorf("invalid character at %d", *off)
		}
		*off = p.pos
	}
	if denominator == 1 {
		return fraction{}, fmt.Errorf("invalid character at %d", *off)
	}
	return fraction{numerator: numerator, denominator: denominator}, nil
}

func checkedMulAdd(n, mul, add uint64) (uint64, error) {
	hi, lo := bits.Mul64(n, mul)
	if hi != 0 {
		return 0, ErrOverflow
	}
	sum, carry := bits.Add64(lo, add, 0)
	if carry != 0 {
		return 0, ErrOverflow
	}
	return sum, nil
}

func checkedMul(n, mul uint64) (uint64, error) {
	return checkedMulAdd(n, mul, 0)
}

func exactDiv(n, d uint64) (uint64, error) {
	if n%d != 0 {
		return 0, ErrOverflow
	}
	return n / d, nil
}

// unitScale is a unit's length: seconds, or nanoseconds for units below a
// second.
var unitScale = map[string]struct {
	secs  uint64
	nanos uint64
}{
	"nanos":   {nanos: 1},
	"nsec":    {nanos: 1},
	"ns":      {nanos: 1},
	"usec":    {nanos: 1000},
	"us":      {nanos: 1000},
	"µs":      {nanos: 1000},
	"millis":  {nanos: 1_000_000},
	"msec":    {nanos: 1_000_000},
	"ms":      {nanos: 1_000_000},
	"seconds": {secs: 1},
	"second":  {secs: 1},
	"secs":    {secs: 1},
	"sec":     {secs: 1},
	"s":       {secs: 1},
	"minutes": {secs: 60},
	"minute":  {secs: 60},
	"min":     {secs: 60},
	"mins":    {secs: 60},
	"m":       {secs: 60},
	"hours":   {secs: 3600},
	"hour":    {secs: 3600},
	"hr":      {secs: 3600},
	"hrs":     {secs: 3600},
	"h":       {secs: 3600},
	"days":    {secs: 86400},
	"day":     {secs: 86400},
	"d":       {secs: 86400},
	"weeks":   {secs: 86400 * 7},
	"week":    {secs: 86400 * 7},
	"wk":      {secs: 86400 * 7},
	"wks":     {secs: 86400 * 7},
	"w":       {secs: 86400 * 7},
	"months":  {secs: 2_630_016},
	"month":   {secs: 2_630_016},
	"M":       {secs: 2_630_016},
	"years":   {secs: 31_557_600},
	"year":    {secs: 31_557_600},
	"yr":      {secs: 31_557_600},
	"yrs":     {secs: 31_557_600},
	"y":       {secs: 31_557_600},
}

func (out *Duration) addUnit(unit []rune, n uint64, frac *fraction) error {
	scale, ok := unitScale[string(unit)]
	if !ok {
		return fmt.Errorf(
			"unknown time unit %q, supported units: ns, us, ms, sec, min, hours, days, weeks, months, years",
			string(unit),
		)
	}
	var secs, nanos uint64
	var err error
	if scale.secs > 0 {
		secs, err = checkedMul(n, scale.secs)
	} else {
		nanos, err = checkedMul(n, scale.nanos)
	}
	if err != nil {
		return err
	}
	if err := out.add(secs, nanos); err != nil {
		return err
	}
	if frac == nil {
		return nil
	}
	secs, nanos = 0, 0
	switch {
	case scale.nanos == 1:
		return ErrOverflow
	case scale.nanos > 0:
		nanos, err = scaledFraction(*frac, scale.nanos)
	case scale.secs == 1 || scale.secs == 60:
		nanos, err = scaledFraction(*frac, scale.secs*1_000_000_000)
	default:
		secs, err = scaledFraction(*frac, scale.secs)
	}
	if err != nil {
		return err
	}
	return out.add(secs, nanos)
}

func scaledFraction(frac fraction, scale uint64) (uint64, error) {
	product, err := checkedMul(frac.numerator, scale)
	if err != nil {
		return 0, err
	}
	return exactDiv(product, frac.denominator)
}

// add mirrors humantime's `add_current`.
func (out *Duration) add(secs, nanos uint64) error {
	total, carry := bits.Add64(uint64(out.Nanoseconds), nanos, 0)
	if carry != 0 {
		return ErrOverflow
	}
	if total > 1_000_000_000 {
		var c uint64
		secs, c = bits.Add64(secs, total/1_000_000_000, 0)
		if c != 0 {
			return ErrOverflow
		}
		total %= 1_000_000_000
	}
	secs, carry = bits.Add64(out.Seconds, secs, 0)
	if carry != 0 {
		return ErrOverflow
	}
	// `Duration::new` carries whole seconds out of the nanoseconds.
	if total >= 1_000_000_000 {
		if secs == math.MaxUint64 {
			return ErrOverflow
		}
		secs++
		total -= 1_000_000_000
	}
	out.Seconds = secs
	out.Nanoseconds = uint32(total)
	return nil
}
