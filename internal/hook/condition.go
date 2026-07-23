package hook

import (
	"encoding/json"
	"fmt"
	"regexp"

	"xagent/internal/matcher"
)

type compiledPredicate struct {
	field       compiledField
	match       MatchType
	stringValue string
	boolValue   bool
	numberValue string
	kind        scalarKind
	negate      bool
	regex       *regexp.Regexp
}

type compiledCondition struct {
	all        bool
	predicates []compiledPredicate
}

func compileCondition(event Event, group *ConditionGroup) (*compiledCondition, error) {
	if group == nil {
		return nil, nil
	}
	items, all := group.Any, false
	if len(group.All) > 0 {
		items, all = group.All, true
	}
	compiled := &compiledCondition{all: all, predicates: make([]compiledPredicate, 0, len(items))}
	for _, item := range items {
		field, err := compileField(event, item.Field)
		if err != nil {
			return nil, err
		}
		p := compiledPredicate{field: field, match: item.Match, negate: item.Negate}
		switch value := item.Value.(type) {
		case string:
			p.kind, p.stringValue = scalarString, value
		case bool:
			p.kind, p.boolValue = scalarBool, value
		case json.Number:
			p.kind = scalarNumber
			p.numberValue = value.String()
		case int:
			p.kind = scalarNumber
			p.numberValue = fmt.Sprint(value)
		case int64:
			p.kind = scalarNumber
			p.numberValue = fmt.Sprint(value)
		case float64:
			p.kind = scalarNumber
			p.numberValue = fmt.Sprint(value)
		default:
			return nil, fmt.Errorf("unsupported predicate value")
		}
		if p.kind == scalarNumber {
			if _, ok := normalizeNumber(p.numberValue); !ok {
				return nil, fmt.Errorf("unsupported predicate value")
			}
		}
		if item.Match != MatchExact && p.kind != scalarString {
			return nil, fmt.Errorf("%s requires string value", item.Match)
		}
		if item.Match == MatchRegex {
			p.regex, err = regexp.Compile(`\A(?:` + p.stringValue + `)\z`)
			if err != nil {
				return nil, fmt.Errorf("invalid regex")
			}
		}
		compiled.predicates = append(compiled.predicates, p)
	}
	return compiled, nil
}

func (c *compiledCondition) matches(event *frozenEvent) (matched bool, err error) {
	if c == nil {
		return true, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			matched = false
			err = fmt.Errorf("condition panic")
		}
	}()
	if c.all {
		for _, p := range c.predicates {
			ok, e := p.matches(event)
			if e != nil {
				return false, e
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	for _, p := range c.predicates {
		ok, e := p.matches(event)
		if e != nil {
			return false, e
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (p compiledPredicate) matches(event *frozenEvent) (bool, error) {
	actual := event.lookup(p.field)
	if !actual.exists || actual.kind == scalarNone || actual.kind != p.kind {
		return false, nil
	}
	var ok bool
	var err error
	switch p.match {
	case MatchExact:
		switch p.kind {
		case scalarString:
			ok = matcher.MatchExact(actual.value.(string), p.stringValue)
		case scalarBool:
			ok = actual.value.(bool) == p.boolValue
		case scalarNumber:
			ok = equalNormalizedNumber(actual.value.(numberScalar).text, p.numberValue)
		}
	case MatchGlob:
		ok, err = matcher.MatchGlob(actual.value.(string), p.stringValue)
	case MatchRegex:
		ok = p.regex.MatchString(actual.value.(string))
	default:
		return false, fmt.Errorf("unknown match type")
	}
	if err != nil {
		return false, err
	}
	if p.negate {
		ok = !ok
	}
	return ok, nil
}
