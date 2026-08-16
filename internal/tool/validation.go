package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"xagent/internal/permission"
	"xagent/internal/safefs"
)

var errToolNotRegistered = errors.New("tool is not registered")

// ValidatedCall is the single parsed representation of a registered tool
// call. Arguments deliberately preserves json.Number values so the same map
// can flow through hooks, permission fingerprints, and actual execution.
type ValidatedCall struct {
	Call      Call
	Tool      Definition
	Arguments map[string]any

	executor           Tool
	canonicalArguments []byte
	workingDirectory   *safefs.Identity
	resourceBindings   []safefs.Binding
	executionRoot      *safefs.Root
	executionRootPath  string
	targetDigest       *[32]byte
	policy             ExecutionPolicy
	provenance         *executorProvenance
}

type ValidationContext struct {
	Root        *safefs.Root
	ProjectRoot string
}

// ValidateCall verifies registry membership and parses exactly one JSON
// object. Tool-specific schema checks remain the responsibility of Tool.Execute.
func (r *Registry) ValidateCall(call Call) (ValidatedCall, error) {
	return r.validateCall(call, nil)
}

// ValidateCallWithContext validates and binds execution resources before an
// authorization identity can be constructed. File tools require an opened
// safefs Root; MCP tools receive the target digest frozen at registration.
func (r *Registry) ValidateCallWithContext(call Call, validation ValidationContext) (ValidatedCall, error) {
	return r.validateCall(call, &validation)
}

func (r *Registry) validateCall(call Call, validation *ValidationContext) (ValidatedCall, error) {
	if r == nil {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	executionTool, ok := r.executionTool(call.Name)
	if !ok {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	registeredTool, ok := r.Get(call.Name)
	if !ok {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	descriptor, ok := r.Descriptor(call.Name)
	if !ok {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	arguments := map[string]any{}
	if strings.TrimSpace(call.ArgumentsJSON) != "" {
		decoder := json.NewDecoder(strings.NewReader(call.ArgumentsJSON))
		decoder.UseNumber()
		if err := decoder.Decode(&arguments); err != nil {
			return ValidatedCall{}, fmt.Errorf("tool arguments must be one JSON object: %w", err)
		}
		if arguments == nil {
			return ValidatedCall{}, fmt.Errorf("tool arguments must be one JSON object")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				err = fmt.Errorf("additional JSON value")
			}
			return ValidatedCall{}, fmt.Errorf("tool arguments must not contain trailing data: %w", err)
		}
	}
	if err := validateArgumentsAgainstSchema(arguments, descriptor.Schema); err != nil {
		return ValidatedCall{}, fmt.Errorf("tool arguments failed schema validation: %w", err)
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return ValidatedCall{}, errors.New("tool arguments cannot be canonicalized")
	}
	validated := ValidatedCall{
		Call:               call,
		Tool:               registeredTool,
		executor:           executionTool,
		Arguments:          arguments,
		canonicalArguments: append([]byte(nil), canonical...),
		targetDigest:       cloneDigest(descriptor.TargetDigest),
		policy:             descriptor.Policy,
	}
	if validation != nil {
		if err := validated.bindResources(*validation); err != nil {
			return ValidatedCall{}, err
		}
	}
	return validated, nil
}

func (v ValidatedCall) CanonicalArguments() []byte {
	return append([]byte(nil), v.canonicalArguments...)
}

func (v ValidatedCall) ResourceBindings() []safefs.Binding {
	return append([]safefs.Binding(nil), v.resourceBindings...)
}

func (v ValidatedCall) TargetDigest() *[32]byte {
	return cloneDigest(v.targetDigest)
}

func (v ValidatedCall) ExecutionPolicy() ExecutionPolicy {
	return v.policy
}

func (v ValidatedCall) IdentityInput() permission.CallIdentityInput {
	return permission.CallIdentityInput{
		ToolName:           v.Call.Name,
		CanonicalArguments: v.CanonicalArguments(),
		WorkingDirectory:   cloneIdentity(v.workingDirectory),
		ResourceBindings:   v.ResourceBindings(),
		TargetDigest:       v.TargetDigest(),
	}
}

func (v *ValidatedCall) bindResources(validation ValidationContext) error {
	if !isFileToolName(v.Call.Name) {
		return nil
	}
	if validation.Root == nil {
		return errors.New("file tool validation requires an opened root")
	}
	identity := validation.Root.Identity()
	if _, err := identity.MarshalBinary(); err != nil {
		return errors.New("file tool validation root is unavailable")
	}
	v.workingDirectory = &identity
	v.executionRoot = validation.Root
	v.executionRootPath = validation.ProjectRoot
	argumentName, binds := bindingArgument(v.Call.Name)
	if !binds {
		return nil
	}
	value, ok := v.Arguments[argumentName].(string)
	if !ok || value == "" {
		return fmt.Errorf("file tool argument %q is invalid", argumentName)
	}
	relative, err := rootRelativeBindingPath(value, validation.ProjectRoot)
	if err != nil {
		return err
	}
	binding, err := validation.Root.Bind(relative)
	if err != nil {
		return fmt.Errorf("%s: file tool resource binding failed", ErrPathOutsideProject)
	}
	v.resourceBindings = []safefs.Binding{binding}
	return nil
}

func isFileToolName(name string) bool {
	switch name {
	case "Read", "Write", "Edit", "Glob", "Grep":
		return true
	default:
		return false
	}
}

func bindingArgument(name string) (string, bool) {
	switch name {
	case "Read", "Write", "Edit":
		return "path", true
	case "Grep":
		return "path", false
	default:
		return "", false
	}
}

func rootRelativeBindingPath(value, projectRoot string) (string, error) {
	if filepath.IsAbs(value) {
		if projectRoot == "" || !filepath.IsAbs(projectRoot) {
			return "", fmt.Errorf("%s: absolute file tool path requires a project root", ErrPathOutsideProject)
		}
		if resolved, err := filepath.EvalSymlinks(value); err == nil {
			value = filepath.Clean(resolved)
		}
		relative, err := filepath.Rel(projectRoot, value)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s: file tool path is outside the opened root", ErrPathOutsideProject)
		}
		value = relative
	}
	return filepath.ToSlash(value), nil
}

func cloneIdentity(identity *safefs.Identity) *safefs.Identity {
	if identity == nil {
		return nil
	}
	cloned := *identity
	return &cloned
}

func validateSchemaDefinition(schema Schema) error {
	document, err := schemaDocument(schema)
	if err != nil {
		return err
	}
	typeValue, ok := document["type"]
	if !ok || !schemaAllowsType(typeValue, "object") {
		return errors.New("tool schema root type must be object")
	}
	return validateSchemaNode(document, 0)
}

func validateArgumentsAgainstSchema(arguments map[string]any, schema Schema) error {
	document, err := schemaDocument(schema)
	if err != nil {
		return err
	}
	return validateSchemaValue("$", arguments, document, 0)
}

func schemaDocument(schema Schema) (map[string]any, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, errors.New("tool schema cannot be encoded")
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, errors.New("tool schema must be one JSON object")
	}
	return document, nil
}

func validateSchemaNode(schema map[string]any, depth int) error {
	if depth > 64 {
		return errors.New("tool schema nesting exceeds limit")
	}
	for _, keyword := range []string{"properties", "$defs", "definitions"} {
		if raw, ok := schema[keyword]; ok {
			children, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("schema keyword %q must be an object", keyword)
			}
			for _, child := range children {
				childSchema, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("schema keyword %q contains a non-object", keyword)
				}
				if err := validateSchemaNode(childSchema, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if raw, ok := schema["items"]; ok {
		if child, ok := raw.(map[string]any); ok {
			if err := validateSchemaNode(child, depth+1); err != nil {
				return err
			}
		} else {
			return errors.New("schema items must be an object")
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		if raw, ok := schema[keyword]; ok {
			items, ok := raw.([]any)
			if !ok || len(items) == 0 {
				return fmt.Errorf("schema keyword %q must be a non-empty array", keyword)
			}
			for _, item := range items {
				child, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("schema keyword %q contains a non-object", keyword)
				}
				if err := validateSchemaNode(child, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateSchemaValue(path string, value any, schema map[string]any, depth int) error {
	if depth > 64 {
		return errors.New("tool argument nesting exceeds limit")
	}
	if reference, ok := schema["$ref"]; ok && reference != nil {
		return fmt.Errorf("%s uses an unsupported schema reference", path)
	}
	if rawType, ok := schema["type"]; ok && !valueMatchesSchemaType(value, rawType) {
		return fmt.Errorf("%s has the wrong JSON type", path)
	}
	if enum, ok := schema["enum"].([]any); ok && !schemaValueInSet(value, enum) {
		return fmt.Errorf("%s is not an allowed enum value", path)
	}
	if constant, ok := schema["const"]; ok && !schemaValuesEqual(value, constant) {
		return fmt.Errorf("%s does not match the required constant", path)
	}
	if err := validateSchemaCombinators(path, value, schema, depth); err != nil {
		return err
	}

	switch typed := value.(type) {
	case map[string]any:
		if err := validateObjectSchema(path, typed, schema, depth); err != nil {
			return err
		}
	case []any:
		if err := validateArraySchema(path, typed, schema, depth); err != nil {
			return err
		}
	case string:
		if err := validateStringSchema(path, typed, schema); err != nil {
			return err
		}
	case json.Number:
		if err := validateNumberSchema(path, typed, schema); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectSchema(path string, value map[string]any, schema map[string]any, depth int) error {
	if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			name, ok := item.(string)
			if !ok {
				return errors.New("schema required contains a non-string")
			}
			if _, exists := value[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, childValue := range value {
		if childRaw, exists := properties[name]; exists {
			childSchema, ok := childRaw.(map[string]any)
			if !ok {
				return fmt.Errorf("schema property %q is invalid", name)
			}
			if err := validateSchemaValue(path+"."+name, childValue, childSchema, depth+1); err != nil {
				return err
			}
			continue
		}
		if additional, exists := schema["additionalProperties"]; exists {
			switch typed := additional.(type) {
			case bool:
				if !typed {
					return fmt.Errorf("%s.%s is not an allowed property", path, name)
				}
			case map[string]any:
				if err := validateSchemaValue(path+"."+name, childValue, typed, depth+1); err != nil {
					return err
				}
			default:
				return errors.New("schema additionalProperties is invalid")
			}
		}
	}
	return nil
}

func validateArraySchema(path string, value []any, schema map[string]any, depth int) error {
	if err := validateLengthKeyword(path, len(value), schema, "minItems", "maxItems"); err != nil {
		return err
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return nil
	}
	for index, item := range value {
		if err := validateSchemaValue(fmt.Sprintf("%s[%d]", path, index), item, items, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateStringSchema(path, value string, schema map[string]any) error {
	if err := validateLengthKeyword(path, utf8.RuneCountInString(value), schema, "minLength", "maxLength"); err != nil {
		return err
	}
	if pattern, ok := schema["pattern"].(string); ok {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return errors.New("schema pattern is invalid")
		}
		if !expression.MatchString(value) {
			return fmt.Errorf("%s does not match the required pattern", path)
		}
	}
	return nil
}

func validateNumberSchema(path string, value json.Number, schema map[string]any) error {
	number, ok := new(big.Rat).SetString(value.String())
	if !ok {
		return fmt.Errorf("%s is not a valid number", path)
	}
	for keyword, compare := range map[string]func(int) bool{
		"minimum": func(result int) bool { return result < 0 },
		"maximum": func(result int) bool { return result > 0 },
	} {
		limitValue, exists := schema[keyword]
		if !exists {
			continue
		}
		limitNumber, ok := limitValue.(json.Number)
		if !ok {
			return fmt.Errorf("schema %s is invalid", keyword)
		}
		limit, ok := new(big.Rat).SetString(limitNumber.String())
		if !ok {
			return fmt.Errorf("schema %s is invalid", keyword)
		}
		if compare(number.Cmp(limit)) {
			return fmt.Errorf("%s violates schema %s", path, keyword)
		}
	}
	return nil
}

func validateLengthKeyword(path string, length int, schema map[string]any, minimum, maximum string) error {
	for keyword, tooFar := range map[string]func(int64) bool{
		minimum: func(limit int64) bool { return int64(length) < limit },
		maximum: func(limit int64) bool { return int64(length) > limit },
	} {
		value, exists := schema[keyword]
		if !exists {
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("schema %s is invalid", keyword)
		}
		limit, err := number.Int64()
		if err != nil || limit < 0 {
			return fmt.Errorf("schema %s is invalid", keyword)
		}
		if tooFar(limit) {
			return fmt.Errorf("%s violates schema %s", path, keyword)
		}
	}
	return nil
}

func validateSchemaCombinators(path string, value any, schema map[string]any, depth int) error {
	for keyword, mode := range map[string]int{"allOf": 0, "anyOf": 1, "oneOf": 2} {
		raw, exists := schema[keyword]
		if !exists {
			continue
		}
		items, ok := raw.([]any)
		if !ok || len(items) == 0 {
			return fmt.Errorf("schema %s is invalid", keyword)
		}
		matches := 0
		for _, item := range items {
			child, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("schema %s is invalid", keyword)
			}
			if validateSchemaValue(path, value, child, depth+1) == nil {
				matches++
			}
		}
		if mode == 0 && matches != len(items) || mode == 1 && matches == 0 || mode == 2 && matches != 1 {
			return fmt.Errorf("%s does not satisfy schema %s", path, keyword)
		}
	}
	return nil
}

func valueMatchesSchemaType(value any, rawType any) bool {
	switch typed := rawType.(type) {
	case string:
		return matchesJSONType(value, typed)
	case []any:
		for _, item := range typed {
			name, ok := item.(string)
			if ok && matchesJSONType(value, name) {
				return true
			}
		}
	}
	return false
}

func matchesJSONType(value any, name string) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		parsed, valid := new(big.Rat).SetString(number.String())
		return valid && parsed.IsInt()
	case "null":
		return value == nil
	default:
		return false
	}
}

func schemaAllowsType(rawType any, name string) bool {
	switch typed := rawType.(type) {
	case string:
		return typed == name
	case []any:
		for _, item := range typed {
			if item == name {
				return true
			}
		}
	}
	return false
}

func schemaValueInSet(value any, candidates []any) bool {
	for _, candidate := range candidates {
		if schemaValuesEqual(value, candidate) {
			return true
		}
	}
	return false
}

func schemaValuesEqual(first, second any) bool {
	left, leftErr := json.Marshal(first)
	right, rightErr := json.Marshal(second)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}
