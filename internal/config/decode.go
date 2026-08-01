package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type decodeProblem string

const (
	decodeInvalidSyntax decodeProblem = "invalid YAML syntax"
	decodeMultipleDocs  decodeProblem = "multiple YAML documents are not allowed"
	decodeUnknownField  decodeProblem = "unknown field"
	decodeDuplicate     decodeProblem = "duplicate field"
	decodeNull          decodeProblem = "null is not allowed"
	decodeWrongType     decodeProblem = "invalid value type"
)

type configDecodeError struct {
	source  string
	field   string
	problem decodeProblem
}

func (e *configDecodeError) Error() string {
	if e.field == "" {
		return fmt.Sprintf("config source %q: %s", e.source, e.problem)
	}
	return fmt.Sprintf("config source %q field %q: %s", e.source, e.field, e.problem)
}

// DecodePartial strictly decodes one configuration layer while preserving
// explicit false, zero, and empty values. It never includes a configuration
// scalar in returned errors.
func DecodePartial(path string) (PartialAppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PartialAppConfig{}, fmt.Errorf("read config source %q failed: %w", path, err)
	}
	return decodePartial(path, data)
}

func decodePartial(source string, data []byte) (PartialAppConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		problem := decodeInvalidSyntax
		if errors.Is(err, io.EOF) {
			problem = decodeWrongType
		}
		return PartialAppConfig{}, newConfigDecodeError(source, "", problem)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return PartialAppConfig{}, newConfigDecodeError(source, "", decodeMultipleDocs)
	} else if !errors.Is(err, io.EOF) {
		return PartialAppConfig{}, newConfigDecodeError(source, "", decodeInvalidSyntax)
	}
	if len(document.Content) != 1 {
		return PartialAppConfig{}, newConfigDecodeError(source, "", decodeWrongType)
	}

	var result PartialAppConfig
	if err := decodeConfigNode(source, "", document.Content[0], reflect.ValueOf(&result).Elem()); err != nil {
		return PartialAppConfig{}, err
	}
	return result, nil
}

func decodeConfigNode(source string, path string, node *yaml.Node, target reflect.Value) error {
	if node == nil || node.Tag == "!!null" {
		return newConfigDecodeError(source, path, decodeNull)
	}
	if value, ok := optionalTarget(target); ok {
		if err := decodeConfigNode(source, path, node, value); err != nil {
			return err
		}
		target.FieldByName("Set").SetBool(true)
		return nil
	}

	switch target.Kind() {
	case reflect.Struct:
		return decodeConfigStruct(source, path, node, target)
	case reflect.Map:
		return decodeConfigMap(source, path, node, target)
	case reflect.Slice:
		return decodeConfigSlice(source, path, node, target)
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		target.SetString(node.Value)
		return nil
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		var value bool
		if err := node.Decode(&value); err != nil {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		target.SetBool(value)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		value := reflect.New(target.Type())
		if err := node.Decode(value.Interface()); err != nil {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		target.Set(value.Elem())
		return nil
	default:
		return newConfigDecodeError(source, path, decodeWrongType)
	}
}

func optionalTarget(target reflect.Value) (reflect.Value, bool) {
	typeOf := target.Type()
	if typeOf.Kind() != reflect.Struct || typeOf.PkgPath() != reflect.TypeOf(Optional[int]{}).PkgPath() ||
		!strings.HasPrefix(typeOf.Name(), "Optional[") || typeOf.NumField() != 2 ||
		typeOf.Field(0).Name != "Set" || typeOf.Field(1).Name != "Value" {
		return reflect.Value{}, false
	}
	return target.FieldByName("Value"), true
}

func decodeConfigStruct(source string, path string, node *yaml.Node, target reflect.Value) error {
	if node.Kind != yaml.MappingNode {
		return newConfigDecodeError(source, path, decodeWrongType)
	}
	fields := make(map[string]int, target.NumField())
	for index := 0; index < target.NumField(); index++ {
		field := target.Type().Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = index
		}
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		fieldPath := joinConfigPath(path, key.Value)
		if _, duplicate := seen[key.Value]; duplicate {
			return newConfigDecodeError(source, fieldPath, decodeDuplicate)
		}
		seen[key.Value] = struct{}{}
		fieldIndex, ok := fields[key.Value]
		if !ok {
			return newConfigDecodeError(source, fieldPath, decodeUnknownField)
		}
		if err := decodeConfigNode(source, fieldPath, value, target.Field(fieldIndex)); err != nil {
			return err
		}
	}
	return nil
}

func decodeConfigMap(source string, path string, node *yaml.Node, target reflect.Value) error {
	if node.Kind != yaml.MappingNode || target.Type().Key().Kind() != reflect.String {
		return newConfigDecodeError(source, path, decodeWrongType)
	}
	result := reflect.MakeMapWithSize(target.Type(), len(node.Content)/2)
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return newConfigDecodeError(source, path, decodeWrongType)
		}
		itemPath := joinConfigPath(path, key.Value)
		if _, duplicate := seen[key.Value]; duplicate {
			return newConfigDecodeError(source, itemPath, decodeDuplicate)
		}
		seen[key.Value] = struct{}{}
		item := reflect.New(target.Type().Elem()).Elem()
		if err := decodeConfigNode(source, itemPath, value, item); err != nil {
			return err
		}
		result.SetMapIndex(reflect.ValueOf(key.Value).Convert(target.Type().Key()), item)
	}
	target.Set(result)
	return nil
}

func decodeConfigSlice(source string, path string, node *yaml.Node, target reflect.Value) error {
	if node.Kind != yaml.SequenceNode {
		return newConfigDecodeError(source, path, decodeWrongType)
	}
	result := reflect.MakeSlice(target.Type(), len(node.Content), len(node.Content))
	for index, item := range node.Content {
		if err := decodeConfigNode(source, path, item, result.Index(index)); err != nil {
			return err
		}
	}
	target.Set(result)
	return nil
}

func joinConfigPath(parent string, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func newConfigDecodeError(source string, field string, problem decodeProblem) error {
	return &configDecodeError{source: source, field: field, problem: problem}
}
