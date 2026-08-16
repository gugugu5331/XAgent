package hook

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"xagent/internal/safefs"
)

type ProjectContext struct {
	Root string `json:"root"`
}
type ExecutionContext struct {
	ID   string        `json:"id"`
	Kind ExecutionKind `json:"kind"`
	Mode HookMode      `json:"mode"`
}
type SessionContext struct {
	ID        string           `json:"id"`
	State     SessionState     `json:"state,omitempty"`
	EndReason SessionEndReason `json:"end_reason,omitempty"`
}
type TurnContext struct {
	ID     string     `json:"id"`
	Status TurnStatus `json:"status,omitempty"`
	Error  string     `json:"error,omitempty"`
}
type MessageContext struct {
	ID      string      `json:"id"`
	Role    MessageRole `json:"role"`
	Content string      `json:"content"`
}
type ToolResultError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}
type ToolResultContext struct {
	Content string           `json:"content"`
	Error   *ToolResultError `json:"error,omitempty"`
}
type ToolContext struct {
	CallID     string             `json:"call_id"`
	Name       string             `json:"name"`
	Arguments  map[string]any     `json:"arguments"`
	Status     ToolStatus         `json:"status,omitempty"`
	DurationMS *int64             `json:"duration_ms,omitempty"`
	Result     *ToolResultContext `json:"result,omitempty"`
}
type CompactContext struct {
	Reason CompactReason `json:"reason"`
	Before CompactStats  `json:"before"`
	Status CompactStatus `json:"status,omitempty"`
	After  *CompactStats `json:"after,omitempty"`
	Error  string        `json:"error,omitempty"`
}

type EventContext struct {
	SchemaVersion int               `json:"schema_version"`
	Event         Event             `json:"event"`
	Sequence      uint64            `json:"sequence"`
	OccurredAt    string            `json:"occurred_at"`
	Project       ProjectContext    `json:"project"`
	Execution     *ExecutionContext `json:"execution,omitempty"`
	Session       *SessionContext   `json:"session,omitempty"`
	Turn          *TurnContext      `json:"turn,omitempty"`
	Message       *MessageContext   `json:"message,omitempty"`
	Tool          *ToolContext      `json:"tool,omitempty"`
	Compact       *CompactContext   `json:"compact,omitempty"`
}

type frozenEvent struct {
	value     EventContext
	json      []byte
	payloadOK bool
}

func (f *frozenEvent) Context() EventContext {
	if f == nil {
		return EventContext{}
	}
	return cloneEventContext(f.value)
}

func (f *frozenEvent) JSON() ([]byte, bool) {
	if f == nil || !f.payloadOK {
		return nil, false
	}
	return append([]byte(nil), f.json...), true
}

func (f *frozenEvent) lookup(path compiledField) fieldValue {
	if f == nil {
		return fieldValue{}
	}
	return lookupEventContext(f.value, path)
}

type eventFactory struct {
	sequence    atomic.Uint64
	clock       func() time.Time
	idSource    func() string
	projectRoot string
	workspaceID safefs.Identity
	limits      Limits
}

func newEventFactory(projectRoot string, limits Limits, clock func() time.Time, idSource func() string) (*eventFactory, error) {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("absolute project root: %w", err)
	}
	if clock == nil {
		clock = time.Now
	}
	if idSource == nil {
		var ids atomic.Uint64
		idSource = func() string { return strconv.FormatUint(ids.Add(1), 36) }
	}
	return &eventFactory{clock: clock, idSource: idSource, projectRoot: filepath.Clean(abs), limits: normalizeLimits(limits)}, nil
}

func (f *eventFactory) bindWorkspaceIdentity(identity safefs.Identity) {
	if f != nil {
		f.workspaceID = identity
	}
}

func (f *eventFactory) validWorkspaceRoot() bool {
	if f == nil {
		return false
	}
	if f.workspaceID == (safefs.Identity{}) {
		return true
	}
	return liveWorkspaceIdentity(f.projectRoot, f.workspaceID)
}

func liveWorkspaceIdentity(projectRoot string, identity safefs.Identity) bool {
	if identity == (safefs.Identity{}) || !canonicalWorkspaceRoot(projectRoot) {
		return false
	}
	opened, err := safefs.Bootstrap(projectRoot, safefs.Policy{})
	if err != nil || opened.Root == nil {
		return false
	}
	defer opened.Root.Close()
	return opened.Root.Identity() == identity
}

func (f *eventFactory) base(event Event) EventContext {
	return EventContext{
		SchemaVersion: 1,
		Event:         event,
		Sequence:      f.sequence.Add(1),
		OccurredAt:    f.clock().UTC().Format(time.RFC3339Nano),
		Project:       ProjectContext{Root: f.projectRoot},
	}
}

func (f *eventFactory) executionBase(event Event, ref ExecutionRef) EventContext {
	c := f.base(event)
	c.Execution = &ExecutionContext{ID: ref.ExecutionID, Kind: ref.Kind, Mode: ref.Mode}
	c.Session = &SessionContext{ID: ref.SessionID}
	c.Turn = &TurnContext{ID: ref.TurnID}
	return c
}

func (f *eventFactory) freeze(c EventContext) *frozenEvent {
	// Size the event before taking a deep copy or asking encoding/json to build an
	// output buffer. This keeps an attacker-controlled oversized message or tool
	// argument from causing multiple full-sized allocations merely to discover
	// that the event cannot be sent to a command or HTTP action.
	size, valid := eventJSONSize(c, f.limits.EventJSONBytes)
	if !valid {
		return &frozenEvent{value: shallowCloneEventContext(c)}
	}
	if size > f.limits.EventJSONBytes {
		return &frozenEvent{value: shallowCloneEventContext(c)}
	}

	snapshot := cloneEventContext(c)
	raw, err := json.Marshal(snapshot)
	if err != nil || len(raw) != size {
		return &frozenEvent{value: snapshot}
	}
	return &frozenEvent{value: snapshot, json: raw, payloadOK: true}
}

// shallowCloneEventContext snapshots all fixed event objects while retaining
// the already-normalized arguments map. It is used only for events that cannot
// produce an action payload. Engine call sites own this map; retaining it avoids
// a second unbounded copy while still allowing conditions to inspect dynamic
// scalar arguments. Context() remains defensive and deep-copies on demand.
func shallowCloneEventContext(in EventContext) EventContext {
	out := in
	if in.Execution != nil {
		v := *in.Execution
		out.Execution = &v
	}
	if in.Session != nil {
		v := *in.Session
		out.Session = &v
	}
	if in.Turn != nil {
		v := *in.Turn
		out.Turn = &v
	}
	if in.Message != nil {
		v := *in.Message
		out.Message = &v
	}
	if in.Tool != nil {
		v := *in.Tool
		if in.Tool.DurationMS != nil {
			duration := *in.Tool.DurationMS
			v.DurationMS = &duration
		}
		if in.Tool.Result != nil {
			r := *in.Tool.Result
			if r.Error != nil {
				e := *r.Error
				r.Error = &e
			}
			v.Result = &r
		}
		out.Tool = &v
	}
	if in.Compact != nil {
		v := *in.Compact
		if v.After != nil {
			a := *v.After
			v.After = &a
		}
		out.Compact = &v
	}
	return out
}

func lookupEventContext(c EventContext, path compiledField) fieldValue {
	stringValue := func(value string) fieldValue {
		return fieldValue{value: value, exists: true, kind: scalarString}
	}
	numberValue := func(value string) fieldValue {
		return numberFieldValue(value)
	}

	switch path.raw {
	case "schema_version":
		return numberValue(strconv.Itoa(c.SchemaVersion))
	case "event":
		return stringValue(string(c.Event))
	case "sequence":
		return numberValue(strconv.FormatUint(c.Sequence, 10))
	case "occurred_at":
		return stringValue(c.OccurredAt)
	case "project.root":
		return stringValue(c.Project.Root)
	case "execution.id":
		if c.Execution != nil {
			return stringValue(c.Execution.ID)
		}
	case "execution.kind":
		if c.Execution != nil {
			return stringValue(string(c.Execution.Kind))
		}
	case "execution.mode":
		if c.Execution != nil {
			return stringValue(string(c.Execution.Mode))
		}
	case "session.id":
		if c.Session != nil {
			return stringValue(c.Session.ID)
		}
	case "session.state":
		if c.Session != nil && c.Session.State != "" {
			return stringValue(string(c.Session.State))
		}
	case "session.end_reason":
		if c.Session != nil && c.Session.EndReason != "" {
			return stringValue(string(c.Session.EndReason))
		}
	case "turn.id":
		if c.Turn != nil {
			return stringValue(c.Turn.ID)
		}
	case "turn.status":
		if c.Turn != nil && c.Turn.Status != "" {
			return stringValue(string(c.Turn.Status))
		}
	case "turn.error":
		if c.Turn != nil && c.Turn.Error != "" {
			return stringValue(c.Turn.Error)
		}
	case "message.id":
		if c.Message != nil {
			return stringValue(c.Message.ID)
		}
	case "message.role":
		if c.Message != nil {
			return stringValue(string(c.Message.Role))
		}
	case "message.content":
		if c.Message != nil {
			return stringValue(c.Message.Content)
		}
	case "tool.call_id":
		if c.Tool != nil {
			return stringValue(c.Tool.CallID)
		}
	case "tool.name":
		if c.Tool != nil {
			return stringValue(c.Tool.Name)
		}
	case "tool.arguments":
		if c.Tool != nil {
			return fieldValue{exists: true, kind: scalarNone}
		}
	case "tool.status":
		if c.Tool != nil && c.Tool.Status != "" {
			return stringValue(string(c.Tool.Status))
		}
	case "tool.duration_ms":
		if c.Tool != nil && c.Tool.DurationMS != nil {
			return numberValue(strconv.FormatInt(*c.Tool.DurationMS, 10))
		}
	case "tool.result.content":
		if c.Tool != nil && c.Tool.Result != nil {
			return stringValue(c.Tool.Result.Content)
		}
	case "tool.result.error.code":
		if c.Tool != nil && c.Tool.Result != nil && c.Tool.Result.Error != nil {
			return stringValue(c.Tool.Result.Error.Code)
		}
	case "tool.result.error.message":
		if c.Tool != nil && c.Tool.Result != nil && c.Tool.Result.Error != nil {
			return stringValue(c.Tool.Result.Error.Message)
		}
	case "tool.result.error.recoverable":
		if c.Tool != nil && c.Tool.Result != nil && c.Tool.Result.Error != nil {
			return fieldValue{value: c.Tool.Result.Error.Recoverable, exists: true, kind: scalarBool}
		}
	case "compact.reason":
		if c.Compact != nil {
			return stringValue(string(c.Compact.Reason))
		}
	case "compact.before.messages":
		if c.Compact != nil {
			return numberValue(strconv.Itoa(c.Compact.Before.Messages))
		}
	case "compact.before.estimated_tokens":
		if c.Compact != nil {
			return numberValue(strconv.Itoa(c.Compact.Before.EstimatedTokens))
		}
	case "compact.status":
		if c.Compact != nil && c.Compact.Status != "" {
			return stringValue(string(c.Compact.Status))
		}
	case "compact.after.messages":
		if c.Compact != nil && c.Compact.After != nil {
			return numberValue(strconv.Itoa(c.Compact.After.Messages))
		}
	case "compact.after.estimated_tokens":
		if c.Compact != nil && c.Compact.After != nil {
			return numberValue(strconv.Itoa(c.Compact.After.EstimatedTokens))
		}
	case "compact.error":
		if c.Compact != nil && c.Compact.Error != "" {
			return stringValue(c.Compact.Error)
		}
	default:
		if c.Tool != nil && len(path.segments) > 2 && path.segments[0] == "tool" && path.segments[1] == "arguments" {
			return lookupSegments(c.Tool.Arguments, path.segments[2:])
		}
	}
	return fieldValue{}
}

type boundedJSONSize struct {
	limit    int
	size     int
	exceeded bool
	invalid  bool
}

func eventJSONSize(c EventContext, limit int) (int, bool) {
	s := &boundedJSONSize{limit: limit}
	s.event(c)
	return s.size, !s.invalid
}

func (s *boundedJSONSize) add(size int) bool {
	if s.invalid || s.exceeded {
		return false
	}
	if size < 0 || size > s.limit-s.size {
		s.size = s.limit + 1
		s.exceeded = true
		return false
	}
	s.size += size
	return true
}

func (s *boundedJSONSize) raw(value string) bool { return s.add(len(value)) }

func (s *boundedJSONSize) quoted(value string) {
	if !s.add(1) {
		return
	}
	for index := 0; index < len(value) && !s.exceeded; {
		b := value[index]
		switch {
		case b < utf8.RuneSelf:
			index++
			switch {
			case b == '\\' || b == '"' || b == '\n' || b == '\r' || b == '\t' || b == '\b' || b == '\f':
				s.add(2)
			case b < 0x20 || b == '<' || b == '>' || b == '&':
				s.add(6)
			default:
				s.add(1)
			}
		default:
			r, width := utf8.DecodeRuneInString(value[index:])
			if r == utf8.RuneError && width == 1 {
				s.add(6)
				index++
				continue
			}
			if r == '\u2028' || r == '\u2029' {
				s.add(6)
			} else {
				s.add(width)
			}
			index += width
		}
	}
	s.add(1)
}

func (s *boundedJSONSize) event(c EventContext) {
	if !s.raw(`{"schema_version":`) {
		return
	}
	s.raw(strconv.Itoa(c.SchemaVersion))
	s.raw(`,"event":`)
	s.quoted(string(c.Event))
	s.raw(`,"sequence":`)
	s.raw(strconv.FormatUint(c.Sequence, 10))
	s.raw(`,"occurred_at":`)
	s.quoted(c.OccurredAt)
	s.raw(`,"project":{"root":`)
	s.quoted(c.Project.Root)
	s.raw(`}`)
	if c.Execution != nil {
		s.raw(`,"execution":{"id":`)
		s.quoted(c.Execution.ID)
		s.raw(`,"kind":`)
		s.quoted(string(c.Execution.Kind))
		s.raw(`,"mode":`)
		s.quoted(string(c.Execution.Mode))
		s.raw(`}`)
	}
	if c.Session != nil {
		s.raw(`,"session":{"id":`)
		s.quoted(c.Session.ID)
		if c.Session.State != "" {
			s.raw(`,"state":`)
			s.quoted(string(c.Session.State))
		}
		if c.Session.EndReason != "" {
			s.raw(`,"end_reason":`)
			s.quoted(string(c.Session.EndReason))
		}
		s.raw(`}`)
	}
	if c.Turn != nil {
		s.raw(`,"turn":{"id":`)
		s.quoted(c.Turn.ID)
		if c.Turn.Status != "" {
			s.raw(`,"status":`)
			s.quoted(string(c.Turn.Status))
		}
		if c.Turn.Error != "" {
			s.raw(`,"error":`)
			s.quoted(c.Turn.Error)
		}
		s.raw(`}`)
	}
	if c.Message != nil {
		s.raw(`,"message":{"id":`)
		s.quoted(c.Message.ID)
		s.raw(`,"role":`)
		s.quoted(string(c.Message.Role))
		s.raw(`,"content":`)
		s.quoted(c.Message.Content)
		s.raw(`}`)
	}
	if c.Tool != nil {
		s.raw(`,"tool":{"call_id":`)
		s.quoted(c.Tool.CallID)
		s.raw(`,"name":`)
		s.quoted(c.Tool.Name)
		s.raw(`,"arguments":`)
		s.dynamic(reflect.ValueOf(c.Tool.Arguments), 0)
		if c.Tool.Status != "" {
			s.raw(`,"status":`)
			s.quoted(string(c.Tool.Status))
		}
		if c.Tool.DurationMS != nil {
			s.raw(`,"duration_ms":`)
			s.raw(strconv.FormatInt(*c.Tool.DurationMS, 10))
		}
		if c.Tool.Result != nil {
			s.raw(`,"result":{"content":`)
			s.quoted(c.Tool.Result.Content)
			if c.Tool.Result.Error != nil {
				s.raw(`,"error":{"code":`)
				s.quoted(c.Tool.Result.Error.Code)
				s.raw(`,"message":`)
				s.quoted(c.Tool.Result.Error.Message)
				s.raw(`,"recoverable":`)
				if c.Tool.Result.Error.Recoverable {
					s.raw("true")
				} else {
					s.raw("false")
				}
				s.raw(`}`)
			}
			s.raw(`}`)
		}
		s.raw(`}`)
	}
	if c.Compact != nil {
		s.raw(`,"compact":{"reason":`)
		s.quoted(string(c.Compact.Reason))
		s.raw(`,"before":{"messages":`)
		s.raw(strconv.Itoa(c.Compact.Before.Messages))
		s.raw(`,"estimated_tokens":`)
		s.raw(strconv.Itoa(c.Compact.Before.EstimatedTokens))
		s.raw(`}`)
		if c.Compact.Status != "" {
			s.raw(`,"status":`)
			s.quoted(string(c.Compact.Status))
		}
		if c.Compact.After != nil {
			s.raw(`,"after":{"messages":`)
			s.raw(strconv.Itoa(c.Compact.After.Messages))
			s.raw(`,"estimated_tokens":`)
			s.raw(strconv.Itoa(c.Compact.After.EstimatedTokens))
			s.raw(`}`)
		}
		if c.Compact.Error != "" {
			s.raw(`,"error":`)
			s.quoted(c.Compact.Error)
		}
		s.raw(`}`)
	}
	s.raw(`}`)
}

var jsonNumberType = reflect.TypeOf(json.Number(""))
var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
var textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()

// dynamic sizes the JSON-normalized values accepted by ToolInput. It emits no
// bytes and stops as soon as the enclosing event exceeds its hard budget.
func (s *boundedJSONSize) dynamic(value reflect.Value, depth int) {
	if s.invalid || s.exceeded {
		return
	}
	if depth > 1_000 {
		s.invalid = true
		return
	}
	if !value.IsValid() {
		s.raw("null")
		return
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			s.raw("null")
			return
		}
		value = value.Elem()
	}
	if value.Type() == jsonNumberType {
		number := value.Interface().(json.Number).String()
		if !validJSONNumber(number) {
			s.invalid = true
			return
		}
		s.raw(number)
		return
	}
	if value.Type().Implements(jsonMarshalerType) || value.Type().Implements(textMarshalerType) ||
		(value.Kind() != reflect.Pointer && reflect.PointerTo(value.Type()).Implements(jsonMarshalerType)) ||
		(value.Kind() != reflect.Pointer && reflect.PointerTo(value.Type()).Implements(textMarshalerType)) {
		s.invalid = true
		return
	}

	switch value.Kind() {
	case reflect.Bool:
		if value.Bool() {
			s.raw("true")
		} else {
			s.raw("false")
		}
	case reflect.String:
		s.quoted(value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s.raw(strconv.FormatInt(value.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		s.raw(strconv.FormatUint(value.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		encoded, err := json.Marshal(value.Interface())
		if err != nil {
			s.invalid = true
			return
		}
		s.add(len(encoded))
	case reflect.Pointer:
		if value.IsNil() {
			s.raw("null")
			return
		}
		s.dynamic(value.Elem(), depth+1)
	case reflect.Map:
		if value.IsNil() {
			s.raw("null")
			return
		}
		if value.Type().Key().Kind() != reflect.String {
			s.invalid = true
			return
		}
		s.raw("{")
		iterator := value.MapRange()
		first := true
		for iterator.Next() && !s.exceeded && !s.invalid {
			if !first {
				s.raw(",")
			}
			first = false
			s.quoted(iterator.Key().String())
			s.raw(":")
			s.dynamic(iterator.Value(), depth+1)
		}
		s.raw("}")
	case reflect.Slice:
		if value.IsNil() {
			s.raw("null")
			return
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			encoded := base64.StdEncoding.EncodedLen(value.Len())
			s.add(encoded + 2)
			return
		}
		s.dynamicArray(value, depth)
	case reflect.Array:
		s.dynamicArray(value, depth)
	default:
		// Tool arguments have already passed strict JSON normalization. Reject
		// non-JSON Go values here instead of invoking arbitrary marshalers that
		// could allocate beyond the event budget.
		s.invalid = true
	}
}

func (s *boundedJSONSize) dynamicArray(value reflect.Value, depth int) {
	s.raw("[")
	for index := 0; index < value.Len() && !s.exceeded && !s.invalid; index++ {
		if index > 0 {
			s.raw(",")
		}
		s.dynamic(value.Index(index), depth+1)
	}
	s.raw("]")
}

func validJSONNumber(value string) bool {
	if value == "" {
		return false
	}
	index := 0
	if value[index] == '-' {
		index++
		if index == len(value) {
			return false
		}
	}
	if value[index] == '0' {
		index++
	} else {
		if value[index] < '1' || value[index] > '9' {
			return false
		}
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
	}
	if index < len(value) && value[index] == '.' {
		index++
		start := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == start {
			return false
		}
	}
	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		index++
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			index++
		}
		start := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == start {
			return false
		}
	}
	return index == len(value)
}

func cloneEventContext(in EventContext) EventContext {
	out := in
	if in.Execution != nil {
		v := *in.Execution
		out.Execution = &v
	}
	if in.Session != nil {
		v := *in.Session
		out.Session = &v
	}
	if in.Turn != nil {
		v := *in.Turn
		out.Turn = &v
	}
	if in.Message != nil {
		v := *in.Message
		out.Message = &v
	}
	if in.Tool != nil {
		v := *in.Tool
		v.Arguments = cloneMap(in.Tool.Arguments)
		if in.Tool.DurationMS != nil {
			duration := *in.Tool.DurationMS
			v.DurationMS = &duration
		}
		if in.Tool.Result != nil {
			r := *in.Tool.Result
			if r.Error != nil {
				e := *r.Error
				r.Error = &e
			}
			v.Result = &r
		}
		out.Tool = &v
	}
	if in.Compact != nil {
		v := *in.Compact
		if v.After != nil {
			a := *v.After
			v.After = &a
		}
		out.Compact = &v
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneValue(x[i])
		}
		return out
	case json.Number:
		return json.Number(x.String())
	default:
		return x
	}
}
