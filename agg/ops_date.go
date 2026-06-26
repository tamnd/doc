package agg

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
)

// evalDate returns the UTC time for a BSON date value, false for anything else.
func evalDate(v bson.RawValue) (time.Time, bool) {
	if v.Type != bson.TypeDateTime {
		return time.Time{}, false
	}
	return time.UnixMilli(v.DateTime()).UTC(), true
}

// loadZone resolves a timezone operand: an Olson name or a fixed UTC offset such
// as "+05:30". An empty string is UTC.
func loadZone(name string) (*time.Location, bool) {
	if name == "" {
		return time.UTC, true
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc, true
	}
	if off, ok := parseOffset(name); ok {
		return time.FixedZone(name, off), true
	}
	return nil, false
}

// parseOffset parses "+HH", "+HH:MM", or "+HHMM" into seconds east of UTC.
func parseOffset(s string) (int, bool) {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	body := strings.ReplaceAll(s[1:], ":", "")
	if len(body) != 2 && len(body) != 4 {
		return 0, false
	}
	h, err := strconv.Atoi(body[:2])
	if err != nil {
		return 0, false
	}
	m := 0
	if len(body) == 4 {
		m, err = strconv.Atoi(body[2:])
		if err != nil {
			return 0, false
		}
	}
	return sign * (h*3600 + m*60), true
}

// dateOptExpr is the {date, timezone} option document common to the date-part
// extractors; tz may be nil.
type dateOptExpr struct {
	date Expr
	tz   Expr
	fn   func(t time.Time) int
}

func (e dateOptExpr) eval(c *evalCtx) bson.RawValue {
	dv := e.date.eval(c)
	if isNullish(dv) {
		return mkNull()
	}
	t, ok := evalDate(dv)
	if !ok {
		return mkNull()
	}
	if e.tz != nil {
		tv := e.tz.eval(c)
		if isNullish(tv) {
			return mkNull()
		}
		name, sok := strOf(tv)
		if !sok {
			return mkNull()
		}
		loc, lok := loadZone(name)
		if !lok {
			return mkNull()
		}
		t = t.In(loc)
	}
	return mkInt32(int32(e.fn(t)))
}

// compileDatePart builds a date-part extractor accepting either a date expression
// or a {date, timezone} document.
func compileDatePart(fn func(t time.Time) int) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		if arg.Type == bson.TypeDocument {
			if dv, ok := arg.Document().Lookup("date"); ok {
				de, err := compileExpr(dv)
				if err != nil {
					return nil, err
				}
				var tz Expr
				if tv, ok := arg.Document().Lookup("timezone"); ok {
					tz, err = compileExpr(tv)
					if err != nil {
						return nil, err
					}
				}
				return dateOptExpr{date: de, tz: tz, fn: fn}, nil
			}
		}
		de, err := compileExpr(arg)
		if err != nil {
			return nil, err
		}
		return dateOptExpr{date: de, fn: fn}, nil
	}
}

// date-part functions matching MongoDB's calendar semantics.
func partYear(t time.Time) int        { return t.Year() }
func partMonth(t time.Time) int       { return int(t.Month()) }
func partDayOfMonth(t time.Time) int  { return t.Day() }
func partHour(t time.Time) int        { return t.Hour() }
func partMinute(t time.Time) int      { return t.Minute() }
func partSecond(t time.Time) int      { return t.Second() }
func partMillisecond(t time.Time) int { return t.Nanosecond() / 1e6 }
func partDayOfYear(t time.Time) int   { return t.YearDay() }

// partDayOfWeek returns 1 (Sunday) through 7 (Saturday), the MongoDB convention.
func partDayOfWeek(t time.Time) int { return int(t.Weekday()) + 1 }

// partIsoDayOfWeek returns 1 (Monday) through 7 (Sunday).
func partIsoDayOfWeek(t time.Time) int {
	wd := int(t.Weekday())
	if wd == 0 {
		return 7
	}
	return wd
}

func partIsoWeek(t time.Time) int {
	_, w := t.ISOWeek()
	return w
}

func partIsoWeekYear(t time.Time) int {
	y, _ := t.ISOWeek()
	return y
}

// partWeek returns the week of the year with weeks starting Sunday, week 0 being
// the days before the first Sunday (MongoDB $week).
func partWeek(t time.Time) int {
	yday := t.YearDay()
	jan1 := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	offset := int(jan1.Weekday())
	return (yday + offset - 1) / 7
}

// ---- $dateToString -------------------------------------------------------

func compileDateToString(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	dv, ok := d.Lookup("date")
	if !ok {
		return nil, ErrBadExpr
	}
	de, err := compileExpr(dv)
	if err != nil {
		return nil, err
	}
	e := dateToStringExpr{date: de, format: "%Y-%m-%dT%H:%M:%S.%LZ"}
	if fv, ok := d.Lookup("format"); ok {
		if s, sok := strOf(fv); sok {
			e.format = s
		}
	}
	if tv, ok := d.Lookup("timezone"); ok {
		e.tz, err = compileExpr(tv)
		if err != nil {
			return nil, err
		}
	}
	if ov, ok := d.Lookup("onNull"); ok {
		e.onNull, err = compileExpr(ov)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

type dateToStringExpr struct {
	date   Expr
	format string
	tz     Expr
	onNull Expr
}

func (e dateToStringExpr) eval(c *evalCtx) bson.RawValue {
	dv := e.date.eval(c)
	if isNullish(dv) {
		if e.onNull != nil {
			return e.onNull.eval(c)
		}
		return mkNull()
	}
	t, ok := evalDate(dv)
	if !ok {
		return mkNull()
	}
	if e.tz != nil {
		tv := e.tz.eval(c)
		if isNullish(tv) {
			return mkNull()
		}
		name, sok := strOf(tv)
		if !sok {
			return mkNull()
		}
		loc, lok := loadZone(name)
		if !lok {
			return mkNull()
		}
		t = t.In(loc)
	}
	return mkString(formatDate(t, e.format))
}

// formatDate renders a time through MongoDB's %-format specifiers.
func formatDate(t time.Time, format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'L':
			fmt.Fprintf(&b, "%03d", t.Nanosecond()/1e6)
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday())+1)
		case 'U':
			fmt.Fprintf(&b, "%02d", partWeek(t))
		case 'G':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%04d", y)
		case 'V':
			_, w := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", w)
		case 'u':
			fmt.Fprintf(&b, "%d", partIsoDayOfWeek(t))
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			_, off := t.Zone()
			fmt.Fprintf(&b, "%d", off/60)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

// ---- $dateFromString -----------------------------------------------------

func compileDateFromString(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	dv, ok := d.Lookup("dateString")
	if !ok {
		return nil, ErrBadExpr
	}
	de, err := compileExpr(dv)
	if err != nil {
		return nil, err
	}
	e := dateFromStringExpr{dateString: de}
	if tv, ok := d.Lookup("timezone"); ok {
		e.tz, err = compileExpr(tv)
		if err != nil {
			return nil, err
		}
	}
	if ov, ok := d.Lookup("onError"); ok {
		e.onError, err = compileExpr(ov)
		if err != nil {
			return nil, err
		}
	}
	if ov, ok := d.Lookup("onNull"); ok {
		e.onNull, err = compileExpr(ov)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

type dateFromStringExpr struct {
	dateString Expr
	tz         Expr
	onError    Expr
	onNull     Expr
}

// isoLayouts are tried in order when no explicit format is given.
var isoLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func (e dateFromStringExpr) eval(c *evalCtx) bson.RawValue {
	sv := e.dateString.eval(c)
	if isNullish(sv) {
		if e.onNull != nil {
			return e.onNull.eval(c)
		}
		return mkNull()
	}
	s, ok := strOf(sv)
	if !ok {
		return e.fail(c)
	}
	loc := time.UTC
	if e.tz != nil {
		tv := e.tz.eval(c)
		if name, sok := strOf(tv); sok {
			if l, lok := loadZone(name); lok {
				loc = l
			}
		}
	}
	for _, layout := range isoLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return mkDate(t.UnixMilli())
		}
	}
	return e.fail(c)
}

func (e dateFromStringExpr) fail(c *evalCtx) bson.RawValue {
	if e.onError != nil {
		return e.onError.eval(c)
	}
	return mkNull()
}

// ---- $dateAdd / $dateSubtract -------------------------------------------

func compileDateAdd(sub bool) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		if arg.Type != bson.TypeDocument {
			return nil, ErrBadExpr
		}
		d := arg.Document()
		sv, ok1 := d.Lookup("startDate")
		uv, ok2 := d.Lookup("unit")
		av, ok3 := d.Lookup("amount")
		if !ok1 || !ok2 || !ok3 {
			return nil, ErrBadExpr
		}
		se, err := compileExpr(sv)
		if err != nil {
			return nil, err
		}
		ue, err := compileExpr(uv)
		if err != nil {
			return nil, err
		}
		ae, err := compileExpr(av)
		if err != nil {
			return nil, err
		}
		e := dateAddExpr{start: se, unit: ue, amount: ae, sub: sub}
		if tv, ok := d.Lookup("timezone"); ok {
			e.tz, err = compileExpr(tv)
			if err != nil {
				return nil, err
			}
		}
		return e, nil
	}
}

type dateAddExpr struct {
	start, unit, amount, tz Expr
	sub                     bool
}

func (e dateAddExpr) eval(c *evalCtx) bson.RawValue {
	sv := e.start.eval(c)
	uv := e.unit.eval(c)
	av := e.amount.eval(c)
	if isNullish(sv) || isNullish(uv) || isNullish(av) {
		return mkNull()
	}
	t, ok := evalDate(sv)
	if !ok {
		return mkNull()
	}
	unit, uok := strOf(uv)
	amount, aok := intArg(av)
	if !uok || !aok {
		return mkNull()
	}
	if e.sub {
		amount = -amount
	}
	if e.tz != nil {
		if name, sok := strOf(e.tz.eval(c)); sok {
			if loc, lok := loadZone(name); lok {
				t = t.In(loc)
			}
		}
	}
	out, ok := addUnit(t, unit, amount)
	if !ok {
		return mkNull()
	}
	return mkDate(out.UTC().UnixMilli())
}

// addUnit shifts a time by amount of the named calendar unit.
func addUnit(t time.Time, unit string, amount int) (time.Time, bool) {
	switch unit {
	case "year":
		return t.AddDate(amount, 0, 0), true
	case "quarter":
		return t.AddDate(0, amount*3, 0), true
	case "month":
		return t.AddDate(0, amount, 0), true
	case "week":
		return t.AddDate(0, 0, amount*7), true
	case "day":
		return t.AddDate(0, 0, amount), true
	case "hour":
		return t.Add(time.Duration(amount) * time.Hour), true
	case "minute":
		return t.Add(time.Duration(amount) * time.Minute), true
	case "second":
		return t.Add(time.Duration(amount) * time.Second), true
	case "millisecond":
		return t.Add(time.Duration(amount) * time.Millisecond), true
	default:
		return time.Time{}, false
	}
}

// ---- $dateDiff -----------------------------------------------------------

func compileDateDiff(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	sv, ok1 := d.Lookup("startDate")
	ev, ok2 := d.Lookup("endDate")
	uv, ok3 := d.Lookup("unit")
	if !ok1 || !ok2 || !ok3 {
		return nil, ErrBadExpr
	}
	se, err := compileExpr(sv)
	if err != nil {
		return nil, err
	}
	ee, err := compileExpr(ev)
	if err != nil {
		return nil, err
	}
	ue, err := compileExpr(uv)
	if err != nil {
		return nil, err
	}
	return dateDiffExpr{start: se, end: ee, unit: ue}, nil
}

type dateDiffExpr struct {
	start, end, unit Expr
}

func (e dateDiffExpr) eval(c *evalCtx) bson.RawValue {
	sv := e.start.eval(c)
	ev := e.end.eval(c)
	uv := e.unit.eval(c)
	if isNullish(sv) || isNullish(ev) || isNullish(uv) {
		return mkNull()
	}
	a, ok1 := evalDate(sv)
	b, ok2 := evalDate(ev)
	unit, ok3 := strOf(uv)
	if !ok1 || !ok2 || !ok3 {
		return mkNull()
	}
	return mkInt64(diffUnit(a, b, unit))
}

// diffUnit returns the number of whole unit boundaries from a to b.
func diffUnit(a, b time.Time, unit string) int64 {
	switch unit {
	case "year":
		return int64(b.Year() - a.Year())
	case "quarter":
		return (int64(b.Year())*4 + int64((int(b.Month())-1)/3)) - (int64(a.Year())*4 + int64((int(a.Month())-1)/3))
	case "month":
		return (int64(b.Year())*12 + int64(b.Month()-1)) - (int64(a.Year())*12 + int64(a.Month()-1))
	case "week":
		return int64(b.Sub(a).Hours()) / (24 * 7)
	case "day":
		return int64(b.Sub(a).Hours()) / 24
	case "hour":
		return int64(b.Sub(a).Hours())
	case "minute":
		return int64(b.Sub(a).Minutes())
	case "second":
		return int64(b.Sub(a).Seconds())
	case "millisecond":
		return b.UnixMilli() - a.UnixMilli()
	default:
		return 0
	}
}

// ---- $dateToParts / $dateFromParts --------------------------------------

func compileDateToParts(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	dv, ok := d.Lookup("date")
	if !ok {
		return nil, ErrBadExpr
	}
	de, err := compileExpr(dv)
	if err != nil {
		return nil, err
	}
	e := dateToPartsExpr{date: de}
	if iv, ok := d.Lookup("iso8601"); ok {
		e.iso = truthy(iv)
	}
	if tv, ok := d.Lookup("timezone"); ok {
		e.tz, err = compileExpr(tv)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

type dateToPartsExpr struct {
	date Expr
	tz   Expr
	iso  bool
}

func (e dateToPartsExpr) eval(c *evalCtx) bson.RawValue {
	dv := e.date.eval(c)
	if isNullish(dv) {
		return mkNull()
	}
	t, ok := evalDate(dv)
	if !ok {
		return mkNull()
	}
	if e.tz != nil {
		if name, sok := strOf(e.tz.eval(c)); sok {
			if loc, lok := loadZone(name); lok {
				t = t.In(loc)
			}
		}
	}
	b := bson.NewBuilder()
	if e.iso {
		iy, iw := t.ISOWeek()
		b.AppendInt32("isoWeekYear", int32(iy)).
			AppendInt32("isoWeek", int32(iw)).
			AppendInt32("isoDayOfWeek", int32(partIsoDayOfWeek(t)))
	} else {
		b.AppendInt32("year", int32(t.Year())).
			AppendInt32("month", int32(t.Month())).
			AppendInt32("day", int32(t.Day()))
	}
	b.AppendInt32("hour", int32(t.Hour())).
		AppendInt32("minute", int32(t.Minute())).
		AppendInt32("second", int32(t.Second())).
		AppendInt32("millisecond", int32(t.Nanosecond()/1e6))
	return mkDoc(b.Build())
}

func compileDateFromParts(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	e := dateFromPartsExpr{}
	fields := []struct {
		key string
		dst *Expr
	}{
		{"year", &e.year}, {"month", &e.month}, {"day", &e.day},
		{"hour", &e.hour}, {"minute", &e.minute}, {"second", &e.second},
		{"millisecond", &e.millisecond}, {"isoWeekYear", &e.isoWeekYear},
		{"isoWeek", &e.isoWeek}, {"isoDayOfWeek", &e.isoDayOfWeek},
		{"timezone", &e.tz},
	}
	for _, f := range fields {
		if v, ok := d.Lookup(f.key); ok {
			ce, err := compileExpr(v)
			if err != nil {
				return nil, err
			}
			*f.dst = ce
		}
	}
	return e, nil
}

type dateFromPartsExpr struct {
	year, month, day     Expr
	hour, minute, second Expr
	millisecond          Expr
	isoWeekYear, isoWeek Expr
	isoDayOfWeek         Expr
	tz                   Expr
}

func (e dateFromPartsExpr) eval(c *evalCtx) bson.RawValue {
	loc := time.UTC
	if e.tz != nil {
		if name, sok := strOf(e.tz.eval(c)); sok {
			if l, lok := loadZone(name); lok {
				loc = l
			}
		}
	}
	part := func(ex Expr, def int) (int, bool) {
		if ex == nil {
			return def, true
		}
		v := ex.eval(c)
		if isNullish(v) {
			return 0, false
		}
		n, ok := intArg(v)
		return n, ok
	}
	hour, ok1 := part(e.hour, 0)
	minute, ok2 := part(e.minute, 0)
	second, ok3 := part(e.second, 0)
	ms, ok4 := part(e.millisecond, 0)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return mkNull()
	}
	if e.isoWeekYear != nil {
		return e.evalISO(c, loc, hour, minute, second, ms)
	}
	year, oky := part(e.year, 1970)
	month, okm := part(e.month, 1)
	day, okd := part(e.day, 1)
	if !oky || !okm || !okd {
		return mkNull()
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, ms*1e6, loc)
	return mkDate(t.UTC().UnixMilli())
}

// evalISO builds a date from ISO week-date parts.
func (e dateFromPartsExpr) evalISO(c *evalCtx, loc *time.Location, hour, minute, second, ms int) bson.RawValue {
	iwy := e.evalInt(c, e.isoWeekYear, 1970)
	iw := e.evalInt(c, e.isoWeek, 1)
	idow := e.evalInt(c, e.isoDayOfWeek, 1)
	// Jan 4 is always in ISO week 1; find that week's Monday then offset.
	jan4 := time.Date(iwy, 1, 4, 0, 0, 0, 0, loc)
	week1Mon := jan4.AddDate(0, 0, -(partIsoDayOfWeek(jan4) - 1))
	t := week1Mon.AddDate(0, 0, (iw-1)*7+(idow-1))
	t = time.Date(t.Year(), t.Month(), t.Day(), hour, minute, second, ms*1e6, loc)
	return mkDate(t.UTC().UnixMilli())
}

func (e dateFromPartsExpr) evalInt(c *evalCtx, ex Expr, def int) int {
	if ex == nil {
		return def
	}
	if n, ok := intArg(ex.eval(c)); ok {
		return n
	}
	return def
}

// ---- $dateTrunc ----------------------------------------------------------

func compileDateTrunc(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	dv, ok1 := d.Lookup("date")
	uv, ok2 := d.Lookup("unit")
	if !ok1 || !ok2 {
		return nil, ErrBadExpr
	}
	de, err := compileExpr(dv)
	if err != nil {
		return nil, err
	}
	ue, err := compileExpr(uv)
	if err != nil {
		return nil, err
	}
	e := dateTruncExpr{date: de, unit: ue, binSize: 1}
	if bv, ok := d.Lookup("binSize"); ok {
		if n, nok := intArg(bv); nok && n > 0 {
			e.binSize = n
		}
	}
	if tv, ok := d.Lookup("timezone"); ok {
		e.tz, err = compileExpr(tv)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

type dateTruncExpr struct {
	date, unit, tz Expr
	binSize        int
}

func (e dateTruncExpr) eval(c *evalCtx) bson.RawValue {
	dv := e.date.eval(c)
	uv := e.unit.eval(c)
	if isNullish(dv) || isNullish(uv) {
		return mkNull()
	}
	t, ok := evalDate(dv)
	if !ok {
		return mkNull()
	}
	unit, uok := strOf(uv)
	if !uok {
		return mkNull()
	}
	if e.tz != nil {
		if name, sok := strOf(e.tz.eval(c)); sok {
			if loc, lok := loadZone(name); lok {
				t = t.In(loc)
			}
		}
	}
	out, ok := truncUnit(t, unit, e.binSize)
	if !ok {
		return mkNull()
	}
	return mkDate(out.UTC().UnixMilli())
}

// truncUnit rounds a time down to the start of the unit bin.
func truncUnit(t time.Time, unit string, bin int) (time.Time, bool) {
	loc := t.Location()
	switch unit {
	case "year":
		y := t.Year() - (t.Year() % bin)
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc), true
	case "quarter":
		q := (int(t.Month()) - 1) / 3
		return time.Date(t.Year(), time.Month(q*3+1), 1, 0, 0, 0, 0, loc), true
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc), true
	case "week":
		back := partIsoDayOfWeek(t) - 1
		d := t.AddDate(0, 0, -back)
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc), true
	case "day":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc), true
	case "hour":
		h := t.Hour() - (t.Hour() % bin)
		return time.Date(t.Year(), t.Month(), t.Day(), h, 0, 0, 0, loc), true
	case "minute":
		m := t.Minute() - (t.Minute() % bin)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), m, 0, 0, loc), true
	case "second":
		s := t.Second() - (t.Second() % bin)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), s, 0, loc), true
	case "millisecond":
		ms := t.Nanosecond() / 1e6
		ms -= ms % bin
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ms*1e6, loc), true
	default:
		return time.Time{}, false
	}
}
