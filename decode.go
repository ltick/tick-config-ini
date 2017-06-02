package ini

import (
	"encoding"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"
)

const (
	documentNode = 1 << iota
	sectionNode
	commentNode
	elementNode
	scalarNode
)

type node struct {
	kind         int
	line, column int
	value        string
	children     []*node
}

// ----------------------------------------------------------------------------
// Parser, produces a node tree out of a ini document.

type parser struct {
	parser ini_parser_t
	event  ini_event_t
	doc    *node
}

func newParser(b []byte) *parser {
	p := parser{}
	if !ini_parser_initialize(&p.parser) {
		panic("failed to initialize INI emitter")
	}

	if len(b) == 0 {
		b = []byte{}
	}

	ini_parser_set_input_string(&p.parser, b)
	p.skip()
	if p.event.typ != ini_DOCUMENT_START_EVENT {
		panic("expected stream start event, got " + strconv.Itoa(int(p.event.typ)))
	}
	return &p
}

func (p *parser) destroy() {
	if p.event.typ != ini_NO_EVENT {
		ini_event_delete(&p.event)
	}
	ini_parser_delete(&p.parser)
}

func (p *parser) skip() {
	if p.event.typ != ini_NO_EVENT {
		if p.event.typ == ini_DOCUMENT_END_EVENT {
			failf("attempted to go past the end of document; corrupted value?")
		}
		ini_event_delete(&p.event)
	}
	if !ini_parser_parse(&p.parser, &p.event) {
		p.fail()
	}
}

func (p *parser) fail() {
	var where string
	var line int
	if p.parser.problem_mark.line != 0 {
		line = p.parser.problem_mark.line
	} else if p.parser.context_mark.line != 0 {
		line = p.parser.context_mark.line
	}
	if line != 0 {
		where = "line " + strconv.Itoa(line) + ": "
	}
	var msg string
	if len(p.parser.problem) > 0 {
		msg = p.parser.problem
	} else {
		msg = "unknown problem parsing INI content"
	}
	failf("%s%s", where, msg)
}

func (p *parser) parse() *node {
	switch p.event.typ {
	case ini_DOCUMENT_START_EVENT:
		return p.document()
	case ini_SECTION_ENTRY_EVENT:
		return p.section()
	case ini_SCALAR_EVENT:
		return p.scalar()
	case ini_COMMENT_EVENT:
		return p.comment()
	case ini_DOCUMENT_END_EVENT:
		// Happens when attempting to decode an empty buffer.
		return nil
	default:
		panic("attempted to parse unknown event: " + strconv.Itoa(int(p.event.typ)))
	}
}

func (p *parser) node(kind int) *node {
	return &node{
		kind:   kind,
		line:   p.event.start_mark.line,
		column: p.event.start_mark.column,
	}
}

func (p *parser) document() *node {
	n := p.node(documentNode)
	p.doc = n
	p.skip()
	n.children = append(n.children, p.parse())
	if p.event.typ != ini_DOCUMENT_END_EVENT {
		panic("expected end of document event but got " + strconv.Itoa(int(p.event.typ)))
	}
	p.skip()
	return n
}

func (p *parser) section() *node {
	n := p.node(sectionNode)
	n.value = string(p.event.value)
	p.skip()
	p.parse()
	if p.event.typ == ini_SECTION_ENTRY_EVENT {
		p.skip()
	} else if p.event.typ == ini_SECTION_INHERIT_EVENT {
		for i := len(p.doc.children) - 1; i >= 0; i-- {
			section := p.doc.children[i]
			if section.value == string(p.event.value) {
				n.children = section.children
			}
		}
	}
	// until next ini_SECTION_ENTRY_EVENT
	for p.event.typ != ini_SECTION_ENTRY_EVENT && p.event.typ != ini_DOCUMENT_END_EVENT{
        fmt.Println(p.event.typ)
        fmt.Println(p.parse())
		//n.children = append(n.children, p.parse())
	}
    fmt.Println("Asdasda")
	p.skip()
	return n
}

func (p *parser) comment() *node {
	n := p.node(commentNode)
	n.value = string(p.event.value)
	p.skip()
	return n
}

func (p *parser) scalar() *node {
	n := p.node(scalarNode)
	n.value = string(p.event.value)
	p.skip()
	return n
}

// ----------------------------------------------------------------------------
// Decoder, unmarshals a node into a provided value.

type decoder struct {
	doc     *node
	aliases map[string]bool
	mapType reflect.Type
	terrors []string
}

var (
	mapItemType    = reflect.TypeOf(MapItem{})
	durationType   = reflect.TypeOf(time.Duration(0))
	defaultMapType = reflect.TypeOf(map[interface{}]interface{}{})
	ifaceType      = defaultMapType.Elem()
)

func newDecoder() *decoder {
	d := &decoder{mapType: defaultMapType}
	d.aliases = make(map[string]bool)
	return d
}

func (d *decoder) terror(n *node, out reflect.Value) {
	value := n.value
	if len(value) > 10 {
		value = " `" + value[:7] + "...`"
	} else {
		value = " `" + value + "`"
	}
	d.terrors = append(d.terrors, fmt.Sprintf("line %d: cannot unmarshal %s into %s", n.line+1, value, out.Type()))
}

func (d *decoder) callUnmarshaler(n *node, u Unmarshaler) (good bool) {
	terrlen := len(d.terrors)
	err := u.UnmarshalINI(func(v interface{}) (err error) {
		defer handleErr(&err)
		d.unmarshal(n, reflect.ValueOf(v))
		if len(d.terrors) > terrlen {
			issues := d.terrors[terrlen:]
			d.terrors = d.terrors[:terrlen]
			return &TypeError{issues}
		}
		return nil
	})
	if e, ok := err.(*TypeError); ok {
		d.terrors = append(d.terrors, e.Errors...)
		return false
	}
	if err != nil {
		fail(err)
	}
	return true
}

// d.prepare initializes and dereferences pointers and calls UnmarshalINI
// if a value is found to implement it.
// It returns the initialized and dereferenced out value, whether
// unmarshalling was already done by UnmarshalINI, and if so whether
// its types unmarshalled appropriately.
//
// If n holds a null value, prepare returns before doing anything.
func (d *decoder) prepare(n *node, out reflect.Value) (newout reflect.Value, unmarshaled, good bool) {
	if n.value == "null" || n.value == "" {
		return out, false, false
	}
	again := true
	for again {
		again = false
		if out.Kind() == reflect.Ptr {
			if out.IsNil() {
				out.Set(reflect.New(out.Type().Elem()))
			}
			out = out.Elem()
			again = true
		}
		if out.CanAddr() {
			if u, ok := out.Addr().Interface().(Unmarshaler); ok {
				good = d.callUnmarshaler(n, u)
				return out, true, good
			}
		}
	}
	return out, false, false
}

func (d *decoder) unmarshal(n *node, out reflect.Value) (good bool) {
	switch n.kind {
	case documentNode:
		return d.document(n, out)
	}
	out, unmarshaled, good := d.prepare(n, out)
	fmt.Println(unmarshaled)
	if unmarshaled {
		return good
	}
	fmt.Println("kind:")
	fmt.Println(n.kind)
	switch n.kind {
	case sectionNode:
		good = d.section(n, out)
	default:
		panic("internal error: unknown node kind: " + strconv.Itoa(n.kind))
	}
	return good
}

func (d *decoder) document(n *node, out reflect.Value) (good bool) {
	if len(n.children) == 1 {
		d.doc = n
		d.unmarshal(n.children[0], out)
		return true
	}
	return false
}

var zeroValue reflect.Value

func resetMap(out reflect.Value) {
	for _, k := range out.MapKeys() {
		out.SetMapIndex(k, zeroValue)
	}
}

func settableValueOf(i interface{}) reflect.Value {
	v := reflect.ValueOf(i)
	sv := reflect.New(v.Type()).Elem()
	sv.Set(v)
	return sv
}

func (d *decoder) section(n *node, out reflect.Value) (good bool) {
	l := len(n.children)

	var iface reflect.Value
	switch out.Kind() {
	case reflect.Slice:
		out.Set(reflect.MakeSlice(out.Type(), l, l))
	case reflect.Interface:
		// No type hints. Will have to use a generic section.
		iface = out
		out = settableValueOf(make([]interface{}, l))
	default:
		d.terror(n, out)
		return false
	}
	et := out.Type().Elem()

	j := 0
	for i := 0; i < l; i++ {
		e := reflect.New(et).Elem()
		if ok := d.unmarshal(n.children[i], e); ok {
			out.Index(j).Set(e)
			j++
		}
	}
	out.Set(out.Slice(0, j))
	if iface.IsValid() {
		iface.Set(out)
	}
	return true
}

func (d *decoder) scalar(n *node, out reflect.Value) (good bool) {
	var resolved interface{}
	if resolved == nil {
		if out.Kind() == reflect.Map && !out.CanAddr() {
			resetMap(out)
		} else {
			out.Set(reflect.Zero(out.Type()))
		}
		return true
	}
	if s, ok := resolved.(string); ok && out.CanAddr() {
		if u, ok := out.Addr().Interface().(encoding.TextUnmarshaler); ok {
			err := u.UnmarshalText([]byte(s))
			if err != nil {
				fail(err)
			}
			return true
		}
	}
	switch out.Kind() {
	case reflect.String:
		if resolved != nil {
			out.SetString(n.value)
			good = true
		}
	case reflect.Interface:
		if resolved == nil {
			out.Set(reflect.Zero(out.Type()))
		} else {
			out.Set(reflect.ValueOf(resolved))
		}
		good = true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch resolved := resolved.(type) {
		case int:
			if !out.OverflowInt(int64(resolved)) {
				out.SetInt(int64(resolved))
				good = true
			}
		case int64:
			if !out.OverflowInt(resolved) {
				out.SetInt(resolved)
				good = true
			}
		case uint64:
			if resolved <= math.MaxInt64 && !out.OverflowInt(int64(resolved)) {
				out.SetInt(int64(resolved))
				good = true
			}
		case float64:
			if resolved <= math.MaxInt64 && !out.OverflowInt(int64(resolved)) {
				out.SetInt(int64(resolved))
				good = true
			}
		case string:
			if out.Type() == durationType {
				d, err := time.ParseDuration(resolved)
				if err == nil {
					out.SetInt(int64(d))
					good = true
				}
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		switch resolved := resolved.(type) {
		case int:
			if resolved >= 0 && !out.OverflowUint(uint64(resolved)) {
				out.SetUint(uint64(resolved))
				good = true
			}
		case int64:
			if resolved >= 0 && !out.OverflowUint(uint64(resolved)) {
				out.SetUint(uint64(resolved))
				good = true
			}
		case uint64:
			if !out.OverflowUint(uint64(resolved)) {
				out.SetUint(uint64(resolved))
				good = true
			}
		case float64:
			if resolved <= math.MaxUint64 && !out.OverflowUint(uint64(resolved)) {
				out.SetUint(uint64(resolved))
				good = true
			}
		}
	case reflect.Bool:
		switch resolved := resolved.(type) {
		case bool:
			out.SetBool(resolved)
			good = true
		}
	case reflect.Float32, reflect.Float64:
		switch resolved := resolved.(type) {
		case int:
			out.SetFloat(float64(resolved))
			good = true
		case int64:
			out.SetFloat(float64(resolved))
			good = true
		case uint64:
			out.SetFloat(float64(resolved))
			good = true
		case float64:
			out.SetFloat(resolved)
			good = true
		}
	case reflect.Ptr:
		if out.Type().Elem() == reflect.TypeOf(resolved) {
			// TODO DOes this make sense? When is out a Ptr except when decoding a nil value?
			elem := reflect.New(out.Type().Elem())
			elem.Elem().Set(reflect.ValueOf(resolved))
			out.Set(elem)
			good = true
		}
	}
	if !good {
		d.terror(n, out)
	}
	return good
}

func (d *decoder) mapping(n *node, out reflect.Value) (good bool) {
	switch out.Kind() {
	case reflect.Struct:
		return d.mappingStruct(n, out)
	case reflect.Slice:
		return d.mappingSlice(n, out)
	case reflect.Map:
		// okay
	case reflect.Interface:
		if d.mapType.Kind() == reflect.Map {
			iface := out
			out = reflect.MakeMap(d.mapType)
			iface.Set(out)
		} else {
			slicev := reflect.New(d.mapType).Elem()
			if !d.mappingSlice(n, slicev) {
				return false
			}
			out.Set(slicev)
			return true
		}
	default:
		d.terror(n, out)
		return false
	}
	outt := out.Type()
	kt := outt.Key()
	et := outt.Elem()

	mapType := d.mapType
	if outt.Key() == ifaceType && outt.Elem() == ifaceType {
		d.mapType = outt
	}

	if out.IsNil() {
		out.Set(reflect.MakeMap(outt))
	}
	l := len(n.children)
	for i := 0; i < l; i += 2 {
		if isMerge(n.children[i]) {
			d.merge(n.children[i+1], out)
			continue
		}
		k := reflect.New(kt).Elem()
		if d.unmarshal(n.children[i], k) {
			kkind := k.Kind()
			if kkind == reflect.Interface {
				kkind = k.Elem().Kind()
			}
			if kkind == reflect.Map || kkind == reflect.Slice {
				failf("invalid map key: %#v", k.Interface())
			}
			e := reflect.New(et).Elem()
			if d.unmarshal(n.children[i+1], e) {
				out.SetMapIndex(k, e)
			}
		}
	}
	d.mapType = mapType
	return true
}

func (d *decoder) mappingSlice(n *node, out reflect.Value) (good bool) {
	outt := out.Type()
	if outt.Elem() != mapItemType {
		d.terror(n, out)
		return false
	}

	mapType := d.mapType
	d.mapType = outt

	var slice []MapItem
	var l = len(n.children)
	for i := 0; i < l; i += 2 {
		if isMerge(n.children[i]) {
			d.merge(n.children[i+1], out)
			continue
		}
		item := MapItem{}
		k := reflect.ValueOf(&item.Key).Elem()
		if d.unmarshal(n.children[i], k) {
			v := reflect.ValueOf(&item.Value).Elem()
			if d.unmarshal(n.children[i+1], v) {
				slice = append(slice, item)
			}
		}
	}
	out.Set(reflect.ValueOf(slice))
	d.mapType = mapType
	return true
}

func (d *decoder) mappingStruct(n *node, out reflect.Value) (good bool) {
	sinfo, err := getStructInfo(out.Type())
	if err != nil {
		panic(err)
	}
	name := settableValueOf("")
	l := len(n.children)

	var inlineMap reflect.Value
	var elemType reflect.Type
	if sinfo.InlineMap != -1 {
		inlineMap = out.Field(sinfo.InlineMap)
		inlineMap.Set(reflect.New(inlineMap.Type()).Elem())
		elemType = inlineMap.Type().Elem()
	}

	for i := 0; i < l; i += 2 {
		ni := n.children[i]
		if isMerge(ni) {
			d.merge(n.children[i+1], out)
			continue
		}
		if !d.unmarshal(ni, name) {
			continue
		}
		if info, ok := sinfo.FieldsMap[name.String()]; ok {
			var field reflect.Value
			if info.Inline == nil {
				field = out.Field(info.Num)
			} else {
				field = out.FieldByIndex(info.Inline)
			}
			d.unmarshal(n.children[i+1], field)
		} else if sinfo.InlineMap != -1 {
			if inlineMap.IsNil() {
				inlineMap.Set(reflect.MakeMap(inlineMap.Type()))
			}
			value := reflect.New(elemType).Elem()
			d.unmarshal(n.children[i+1], value)
			inlineMap.SetMapIndex(name, value)
		}
	}
	return true
}

func failWantMap() {
	failf("map merge requires map or sequence of maps as the value")
}

func (d *decoder) merge(n *node, out reflect.Value) {
	switch n.kind {
	case sectionNode:
		// Step backwards as earlier nodes take precedence.
		for i := len(n.children) - 1; i >= 0; i-- {
			ni := n.children[i]
			d.unmarshal(ni, out)
		}
	default:
		failWantMap()
	}
}

func isMerge(n *node) bool {
	return n.kind == elementNode
}
